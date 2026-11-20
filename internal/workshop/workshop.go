// Package workshop manages interactive GPU sessions ("workshops") on RunPod.
//
// RunPod is the source of truth: a workshop is a pod whose name starts with
// "ws-", and everything the toolkit needs to know about it (budget deadline,
// price, idle timeout) is stored in the pod's environment at creation. There
// is no local state that could drift.
package workshop

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"toolkit/internal/runpod"
)

const (
	podNamePrefix = "ws-"
	image         = "runpod/pytorch:1.0.2-cu1281-torch280-ubuntu2404"
	containerDisk = 50 // GB, deleted with the pod
)

// Pod environment variables. The guardian and the in-session prompt read them
// on the pod; list and close read them back through the API.
const (
	envName        = "WS_NAME"
	envCreated     = "WS_CREATED"  // unix seconds
	envDeadline    = "WS_DEADLINE" // unix seconds; the guardian deletes the pod at this time
	envPrice       = "WS_PRICE_PER_HOUR"
	envBudget      = "WS_BUDGET" // as the user typed it, for display
	envIdleSeconds = "WS_IDLE_SECONDS"
	envGuardian    = "WS_GUARDIAN" // base64 guardian script
)

//go:embed guardian.sh
var guardianScript string

// The pod's command is overridden to start the guardian before the image's
// normal startup script, which sets up sshd with PUBLIC_KEY. Starting it from
// the container command (rather than over SSH after creation) means a pod can
// never run unguarded, even if the laptop disappears right after creating it.
const bootstrapScript = `mkdir -p /etc/workshop
echo "$WS_GUARDIAN" | base64 -d > /etc/workshop/guardian.sh
chmod +x /etc/workshop/guardian.sh
nohup /etc/workshop/guardian.sh >> /var/log/workshop-guardian.log 2>&1 &
exec /start.sh`

var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

type OpenOptions struct {
	Name        string
	GPU         string // catalog id or name, e.g. "RTX 4090"
	MaxPrice    float64
	Budget      Budget
	IdleTimeout time.Duration
	Community   bool
	SSHKey      string // path to the public key
	Debug       bool   // print each SSH probe command with its exit code and output
}

func Open(ctx context.Context, client *runpod.Client, opts OpenOptions) error {
	if !validName.MatchString(opts.Name) {
		return fmt.Errorf("invalid workshop name %q: use lowercase letters, digits and dashes", opts.Name)
	}

	publicKey, err := os.ReadFile(opts.SSHKey)
	if err != nil {
		return fmt.Errorf("reading SSH public key: %w", err)
	}
	if err := checkSSHKey(opts.SSHKey); err != nil {
		return err
	}

	existing, err := findWorkshop(ctx, client, opts.Name)
	if err != nil && !errors.Is(err, errNotFound) {
		return err
	}
	if err == nil {
		return fmt.Errorf("workshop %q is already open (%s). Reconnect with: toolkit workshop connect %s", opts.Name, existing.Status, opts.Name)
	}

	cloud := "SECURE"
	if opts.Community {
		cloud = "COMMUNITY"
	}
	catalog, err := client.ListGPUTypes(ctx, cloud)
	if err != nil {
		return fmt.Errorf("reading GPU catalog: %w", err)
	}
	gpu, err := chooseGPU(catalog, opts.GPU, cloud, opts.MaxPrice)
	if err != nil {
		return err
	}
	price := gpuPrice(gpu, cloud)

	created := time.Now()
	budgetTime := opts.Budget.Duration(price)
	deadline := created.Add(budgetTime)

	fmt.Printf("Creating workshop %q on %s (%s cloud, $%.2f/h)...\n", opts.Name, gpu.Name, strings.ToLower(cloud), price)
	pod, err := client.CreatePod(ctx, runpod.CreatePodRequest{
		Name:  podNamePrefix + opts.Name,
		Image: image,
		GPU:   runpod.GPURequest{ID: gpu.ID, Count: 1},
		Cloud: cloud,
		Disk:  containerDisk,
		Ports: []string{"22/tcp"},
		Env: map[string]string{
			"PUBLIC_KEY":   strings.TrimSpace(string(publicKey)),
			envName:        opts.Name,
			envCreated:     strconv.FormatInt(created.Unix(), 10),
			envDeadline:    strconv.FormatInt(deadline.Unix(), 10),
			envPrice:       strconv.FormatFloat(price, 'f', 4, 64),
			envBudget:      opts.Budget.String(),
			envIdleSeconds: strconv.Itoa(int(opts.IdleTimeout.Seconds())),
			envGuardian:    base64.StdEncoding.EncodeToString([]byte(guardianScript)),
		},
		Cmd: []string{"bash", "-c", bootstrapScript},
	})
	if err != nil {
		var apiErr *runpod.APIError
		if errors.As(err, &apiErr) && apiErr.Status == 400 {
			// RunPod reports "no capacity" as a plain 400 with a human-readable detail.
			fmt.Printf("RunPod could not create the pod: %s\n", apiErr.Detail)
			printAlternatives(catalog, gpu.ID, cloud, opts.MaxPrice)
			return errors.New("workshop not created")
		}
		return fmt.Errorf("creating pod: %w", err)
	}
	if pod.Cost > opts.MaxPrice {
		fmt.Printf("Warning: RunPod reports $%.2f/h for this pod, above your max of $%.2f/h. Close it if that's not acceptable.\n", pod.Cost, opts.MaxPrice)
	}

	// Ctrl-C while waiting stops the wait, not the toolkit, so the pod can
	// still be cleaned up below.
	waitCtx, stopWaiting := signal.NotifyContext(ctx, os.Interrupt)
	fmt.Print("Waiting for SSH")
	readyPod, endpoint, err := waitForSSH(waitCtx, client, pod.ID, opts.SSHKey, opts.Debug)
	stopWaiting()
	fmt.Println()
	if err != nil {
		// When we give up we can't tell whether the container, and so the
		// guardian, ever started: delete the pod rather than leave it unprotected.
		fmt.Printf("Giving up: %v\n", err)
		return deleteAbandonedPod(client, pod, opts.Name, created)
	}
	pod = readyPod

	fmt.Println()
	fmt.Printf("  Workshop  %s\n", opts.Name)
	fmt.Printf("  GPU       %s (%d GB)\n", gpu.Name, gpu.Memory)
	fmt.Printf("  Price     $%.2f/h\n", pod.Cost)
	fmt.Printf("  Budget    %s: deleted at %s (est. $%.2f)\n", opts.Budget, deadline.Format("15:04"), price*budgetTime.Hours())
	fmt.Printf("  Idle      deleted after %s with no SSH session and GPU at rest\n", formatDuration(opts.IdleTimeout))
	fmt.Println()

	return runInteractiveSSH(ctx, client, pod, endpoint, opts.SSHKey)
}

func deleteAbandonedPod(client *runpod.Client, pod runpod.Pod, name string, created time.Time) error {
	// A fresh context: the caller's may be the one Ctrl-C just cancelled.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := client.DeletePod(ctx, pod.ID)
	var apiErr *runpod.APIError
	if err != nil && !(errors.As(err, &apiErr) && apiErr.Status == 404) {
		return fmt.Errorf("could not delete pod %s (%v). It may keep costing money: delete it with 'toolkit workshop close %s' or from the RunPod console", pod.ID, err, name)
	}
	forgetHostKey(pod.ID)
	fmt.Printf("Deleted pod %s, so nothing is left running (est. cost ~$%.2f).\n", pod.ID, pod.Cost*time.Since(created).Hours())
	return errors.New("workshop not opened")
}

func Connect(ctx context.Context, client *runpod.Client, name, sshKey string) error {
	pod, err := findWorkshop(ctx, client, name)
	if err != nil {
		return err
	}
	if pod.Status != "RUNNING" {
		return fmt.Errorf("workshop %q is %s, not RUNNING", name, pod.Status)
	}
	// List results don't include live connection details; the single pod does.
	pod, err = client.GetPod(ctx, pod.ID)
	if err != nil {
		return err
	}
	if pod.SSH.Direct == nil {
		return fmt.Errorf("workshop %q has no public SSH port yet, try again in a moment", name)
	}
	return runInteractiveSSH(ctx, client, pod, *pod.SSH.Direct, sshKey)
}

func List(ctx context.Context, client *runpod.Client) error {
	workshops, err := listWorkshops(ctx, client)
	if err != nil {
		return err
	}
	if len(workshops) == 0 {
		fmt.Println("No open workshops.")
		return nil
	}

	now := time.Now()
	table := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "NAME\tSTATUS\tGPU\tELAPSED\tEST. COST\tBUDGET")
	for _, pod := range workshops {
		info := readWorkshopEnv(pod)
		elapsed := now.Sub(info.created)
		gpuName := "-"
		if pod.GPU != nil {
			gpuName = pod.GPU.ID
		}
		budget := fmt.Sprintf("%s (deleted at %s)", info.budget, info.deadline.Format("15:04"))
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t~$%.2f\t%s\n",
			strings.TrimPrefix(pod.Name, podNamePrefix), pod.Status, gpuName,
			formatDuration(elapsed), pod.Cost*elapsed.Hours(), budget)
	}
	return table.Flush()
}

func Close(ctx context.Context, client *runpod.Client, name string, skipConfirmation bool) error {
	pod, err := findWorkshop(ctx, client, name)
	if err != nil {
		return err
	}

	if !skipConfirmation {
		fmt.Printf("Delete workshop %q? Everything on the machine will be lost. [y/N] ", name)
		answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer != "y" && answer != "yes" {
			fmt.Println("Cancelled.")
			return nil
		}
	}

	if err := client.DeletePod(ctx, pod.ID); err != nil {
		return fmt.Errorf("deleting pod: %w", err)
	}
	forgetHostKey(pod.ID)

	info := readWorkshopEnv(pod)
	duration := time.Since(info.created)
	fmt.Printf("Workshop %q deleted.\n", name)
	fmt.Printf("  Duration   %s\n", formatDuration(duration))
	fmt.Printf("  Est. cost  ~$%.2f ($%.2f/h)\n", pod.Cost*duration.Hours(), pod.Cost)
	return nil
}

// GPU selection

// chooseGPU finds the requested GPU in the catalog and checks that it can be
// rented within the max price. When it can't, it prints alternatives that can.
func chooseGPU(catalog []runpod.GPUType, wanted, cloud string, maxPrice float64) (runpod.GPUType, error) {
	var matches []runpod.GPUType
	for _, gpu := range catalog {
		if strings.EqualFold(gpu.ID, wanted) || strings.EqualFold(gpu.Name, wanted) {
			matches = []runpod.GPUType{gpu}
			break
		}
		if strings.Contains(strings.ToLower(gpu.ID), strings.ToLower(wanted)) {
			matches = append(matches, gpu)
		}
	}

	switch {
	case len(matches) == 0:
		fmt.Printf("No GPU named %q in the RunPod catalog.\n", wanted)
		printAlternatives(catalog, "", cloud, maxPrice)
		return runpod.GPUType{}, errors.New("unknown GPU")
	case len(matches) > 1:
		var names []string
		for _, gpu := range matches {
			names = append(names, gpu.Name)
		}
		return runpod.GPUType{}, fmt.Errorf("%q matches several GPUs: %s", wanted, strings.Join(names, ", "))
	}

	gpu := matches[0]
	price := gpuPrice(gpu, cloud)
	switch {
	case !offeredIn(gpu, cloud):
		fmt.Printf("%s is not offered in the %s cloud.\n", gpu.Name, strings.ToLower(cloud))
	case gpu.Availability == "NONE" || gpu.Availability == "":
		fmt.Printf("%s is not available right now.\n", gpu.Name)
	case price > maxPrice:
		fmt.Printf("%s costs $%.2f/h, above your max of $%.2f/h.\n", gpu.Name, price, maxPrice)
	default:
		return gpu, nil
	}
	printAlternatives(catalog, gpu.ID, cloud, maxPrice)
	return runpod.GPUType{}, errors.New("requested GPU not available within max price")
}

func printAlternatives(catalog []runpod.GPUType, exclude, cloud string, maxPrice float64) {
	var affordable, tooExpensive []runpod.GPUType
	for _, gpu := range catalog {
		if gpu.ID == exclude || !offeredIn(gpu, cloud) || gpu.Availability == "NONE" || gpu.Availability == "" {
			continue
		}
		if gpuPrice(gpu, cloud) <= maxPrice {
			affordable = append(affordable, gpu)
		} else {
			tooExpensive = append(tooExpensive, gpu)
		}
	}
	byPrice := func(gpus []runpod.GPUType) {
		sort.Slice(gpus, func(i, j int) bool { return gpuPrice(gpus[i], cloud) < gpuPrice(gpus[j], cloud) })
	}
	byPrice(affordable)
	byPrice(tooExpensive)

	if len(affordable) > 0 {
		fmt.Printf("Available now within $%.2f/h:\n", maxPrice)
		printGPUs(affordable, cloud, 8)
		return
	}
	if len(tooExpensive) > 0 {
		fmt.Printf("Nothing available within $%.2f/h. Cheapest available (raise --max-price):\n", maxPrice)
		printGPUs(tooExpensive, cloud, 3)
		return
	}
	fmt.Printf("No GPU is available in the %s cloud right now.\n", strings.ToLower(cloud))
}

func printGPUs(gpus []runpod.GPUType, cloud string, limit int) {
	table := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for i, gpu := range gpus {
		if i == limit {
			break
		}
		fmt.Fprintf(table, "  %s\t%d GB\t$%.2f/h\t%s availability\t--gpu %q\n",
			gpu.Name, gpu.Memory, gpuPrice(gpu, cloud), strings.ToLower(gpu.Availability), gpu.ID)
	}
	table.Flush()
}

func offeredIn(gpu runpod.GPUType, cloud string) bool {
	if cloud == "COMMUNITY" {
		return gpu.Community
	}
	return gpu.Secure
}

func gpuPrice(gpu runpod.GPUType, cloud string) float64 {
	if cloud == "COMMUNITY" {
		return gpu.Price.Community
	}
	return gpu.Price.Secure
}

// Finding workshops

var errNotFound = errors.New("workshop not found")

func listWorkshops(ctx context.Context, client *runpod.Client) ([]runpod.Pod, error) {
	pods, err := client.ListPods(ctx)
	if err != nil {
		return nil, err
	}
	var workshops []runpod.Pod
	for _, pod := range pods {
		// The env check keeps pods that merely happen to start with "ws-" out.
		if strings.HasPrefix(pod.Name, podNamePrefix) && pod.Env[envDeadline] != "" {
			workshops = append(workshops, pod)
		}
	}
	return workshops, nil
}

func findWorkshop(ctx context.Context, client *runpod.Client, name string) (runpod.Pod, error) {
	workshops, err := listWorkshops(ctx, client)
	if err != nil {
		return runpod.Pod{}, err
	}
	for _, pod := range workshops {
		if pod.Name == podNamePrefix+name {
			return pod, nil
		}
	}
	return runpod.Pod{}, fmt.Errorf("%w: %q (see: toolkit workshop list)", errNotFound, name)
}

type workshopEnv struct {
	created  time.Time
	deadline time.Time
	budget   string
}

func readWorkshopEnv(pod runpod.Pod) workshopEnv {
	info := workshopEnv{created: pod.CreatedAt, budget: pod.Env[envBudget]}
	if seconds, err := strconv.ParseInt(pod.Env[envCreated], 10, 64); err == nil {
		info.created = time.Unix(seconds, 0)
	}
	if seconds, err := strconv.ParseInt(pod.Env[envDeadline], 10, 64); err == nil {
		info.deadline = time.Unix(seconds, 0)
	}
	return info
}

// Budget

// Budget is either a duration or an amount of dollars. A dollar budget is
// turned into a duration at the GPU's hourly price when the workshop opens.
type Budget struct {
	time    time.Duration
	dollars float64
}

// ParseBudget accepts a duration ("90m", "2h") or dollars ("$5", "5$", "5usd").
func ParseBudget(s string) (Budget, error) {
	trimmed := strings.ToLower(strings.TrimSpace(s))
	amount := strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(trimmed, "$"), "$"), "usd")
	if amount != trimmed {
		dollars, err := strconv.ParseFloat(amount, 64)
		if err != nil || dollars <= 0 {
			return Budget{}, fmt.Errorf("invalid dollar budget %q", s)
		}
		return Budget{dollars: dollars}, nil
	}
	duration, err := time.ParseDuration(trimmed)
	if err != nil || duration <= 0 {
		return Budget{}, fmt.Errorf("invalid budget %q: use a duration like 2h or 90m, or dollars like $5", s)
	}
	return Budget{time: duration}, nil
}

func (b Budget) Duration(pricePerHour float64) time.Duration {
	if b.dollars > 0 {
		return time.Duration(b.dollars / pricePerHour * float64(time.Hour))
	}
	return b.time
}

func (b Budget) String() string {
	if b.dollars > 0 {
		return fmt.Sprintf("$%.2f", b.dollars)
	}
	return formatDuration(b.time)
}

func formatDuration(d time.Duration) string {
	minutes := int(d.Round(time.Minute).Minutes())
	return fmt.Sprintf("%dh%02d", minutes/60, minutes%60)
}

// Package workshop manages interactive GPU sessions ("workshops") on RunPod.
//
// RunPod is the source of truth: a workshop is a pod whose name starts with
// "ws-", and everything the toolkit needs to know about it (budget deadline,
// price, idle timeout) is stored in the pod's environment at creation. There
// is no local state that could drift.
//
// This package does not print anything: it returns data, typed errors and
// progress events, and the command line decides how to show them.
package workshop

import (
	"context"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
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

func ValidateName(name string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("invalid workshop name %q: use lowercase letters, digits and dashes", name)
	}
	return nil
}

// Workshops

// Info is what the toolkit knows about a workshop, read back from its pod.
type Info struct {
	Name        string
	Pod         runpod.Pod
	Created     time.Time
	Deadline    time.Time // the guardian deletes the pod at this time
	Budget      string    // as the user typed it, for display
	IdleTimeout time.Duration
}

func infoOf(pod runpod.Pod) Info {
	info := Info{
		Name:    strings.TrimPrefix(pod.Name, podNamePrefix),
		Pod:     pod,
		Created: pod.CreatedAt,
		Budget:  pod.Env[envBudget],
	}
	if seconds, err := strconv.ParseInt(pod.Env[envCreated], 10, 64); err == nil {
		info.Created = time.Unix(seconds, 0)
	}
	if seconds, err := strconv.ParseInt(pod.Env[envDeadline], 10, 64); err == nil {
		info.Deadline = time.Unix(seconds, 0)
	}
	if seconds, err := strconv.Atoi(pod.Env[envIdleSeconds]); err == nil {
		info.IdleTimeout = time.Duration(seconds) * time.Second
	}
	return info
}

func (i Info) Elapsed() time.Duration {
	return time.Since(i.Created)
}

// EstimatedCost is the hourly price times the time since creation: RunPod
// doesn't report what a running pod has cost so far.
func (i Info) EstimatedCost() float64 {
	return i.Pod.Cost * i.Elapsed().Hours()
}

func (i Info) BudgetReached() bool {
	return !i.Deadline.IsZero() && !time.Now().Before(i.Deadline)
}

var ErrNotFound = errors.New("workshop not found")

func List(ctx context.Context, client *runpod.Client) ([]Info, error) {
	pods, err := client.ListPods(ctx)
	if err != nil {
		return nil, err
	}
	var workshops []Info
	for _, pod := range pods {
		// The env check keeps pods that merely happen to start with "ws-" out.
		if strings.HasPrefix(pod.Name, podNamePrefix) && pod.Env[envDeadline] != "" {
			workshops = append(workshops, infoOf(pod))
		}
	}
	return workshops, nil
}

func Find(ctx context.Context, client *runpod.Client, name string) (Info, error) {
	workshops, err := List(ctx, client)
	if err != nil {
		return Info{}, err
	}
	for _, workshop := range workshops {
		if workshop.Name == name {
			return workshop, nil
		}
	}
	return Info{}, fmt.Errorf("%w: %q (see: toolkit workshop list)", ErrNotFound, name)
}

// Spec describes a workshop to create.
type Spec struct {
	Name        string
	GPU         runpod.GPUType
	Cloud       string // "SECURE" or "COMMUNITY"
	Budget      Budget
	IdleTimeout time.Duration
	PublicKey   string // contents of the SSH public key file
}

// CapacityError means RunPod refused to create the pod, usually because no
// machine with that GPU is free right now. Another GPU may work.
type CapacityError struct {
	GPU    runpod.GPUType
	Detail string // RunPod's own message
}

func (e *CapacityError) Error() string {
	return fmt.Sprintf("RunPod could not create a pod with %s: %s", e.GPU.Name, e.Detail)
}

// Create creates the workshop's pod. It returns as soon as RunPod has accepted
// it; WaitUntilReady waits for the machine to boot.
func Create(ctx context.Context, client *runpod.Client, spec Spec) (Info, error) {
	price := Price(spec.GPU, spec.Cloud)
	created := time.Now()
	deadline := created.Add(spec.Budget.Duration(price))

	pod, err := client.CreatePod(ctx, runpod.CreatePodRequest{
		Name:  podNamePrefix + spec.Name,
		Image: image,
		GPU:   runpod.GPURequest{ID: spec.GPU.ID, Count: 1},
		Cloud: spec.Cloud,
		Disk:  containerDisk,
		Ports: []string{"22/tcp"},
		Env: map[string]string{
			"PUBLIC_KEY":   strings.TrimSpace(spec.PublicKey),
			envName:        spec.Name,
			envCreated:     strconv.FormatInt(created.Unix(), 10),
			envDeadline:    strconv.FormatInt(deadline.Unix(), 10),
			envPrice:       strconv.FormatFloat(price, 'f', 4, 64),
			envBudget:      spec.Budget.String(),
			envIdleSeconds: strconv.Itoa(int(spec.IdleTimeout.Seconds())),
			envGuardian:    base64.StdEncoding.EncodeToString([]byte(guardianScript)),
		},
		Cmd: []string{"bash", "-c", bootstrapScript},
	})
	if err != nil {
		var apiErr *runpod.APIError
		if errors.As(err, &apiErr) && apiErr.Status == 400 {
			// RunPod reports "no capacity" as a plain 400 with a human-readable detail.
			return Info{}, &CapacityError{GPU: spec.GPU, Detail: apiErr.Detail}
		}
		return Info{}, fmt.Errorf("creating pod: %w", err)
	}

	// Use the values we just sent rather than trusting the response to echo
	// the environment back.
	return Info{
		Name:        spec.Name,
		Pod:         pod,
		Created:     created,
		Deadline:    deadline,
		Budget:      spec.Budget.String(),
		IdleTimeout: spec.IdleTimeout,
	}, nil
}

// Delete terminates the workshop's pod. A pod that is already gone counts as
// deleted.
func Delete(ctx context.Context, client *runpod.Client, info Info) error {
	err := client.DeletePod(ctx, info.Pod.ID)
	var apiErr *runpod.APIError
	if err != nil && !(errors.As(err, &apiErr) && apiErr.Status == 404) {
		return fmt.Errorf("deleting pod %s: %w", info.Pod.ID, err)
	}
	forgetHostKey(info.Pod.ID)
	return nil
}

// GPU selection

// UnavailableGPUError explains why the requested GPU can't be used.
type UnavailableGPUError struct {
	Reason  string           // a full sentence, e.g. "RTX 4090 is not available right now."
	Matches []runpod.GPUType // set when the requested name matched several GPUs
}

func (e *UnavailableGPUError) Error() string {
	return e.Reason
}

// MatchGPU finds the requested GPU in the catalog, by exact id or name, or by
// a unique partial id ("4090"), and checks it can be rented within maxPrice.
func MatchGPU(catalog []runpod.GPUType, wanted, cloud string, maxPrice float64) (runpod.GPUType, error) {
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
		return runpod.GPUType{}, &UnavailableGPUError{Reason: fmt.Sprintf("No GPU named %q in the RunPod catalog.", wanted)}
	case len(matches) > 1:
		var names []string
		for _, gpu := range matches {
			names = append(names, gpu.Name)
		}
		return runpod.GPUType{}, &UnavailableGPUError{
			Reason:  fmt.Sprintf("%q matches several GPUs: %s.", wanted, strings.Join(names, ", ")),
			Matches: matches,
		}
	}

	gpu := matches[0]
	price := Price(gpu, cloud)
	switch {
	case !offeredIn(gpu, cloud):
		return runpod.GPUType{}, &UnavailableGPUError{Reason: fmt.Sprintf("%s is not offered in the %s cloud.", gpu.Name, strings.ToLower(cloud))}
	case !available(gpu):
		return runpod.GPUType{}, &UnavailableGPUError{Reason: fmt.Sprintf("%s is not available right now.", gpu.Name)}
	case price > maxPrice:
		return runpod.GPUType{}, &UnavailableGPUError{Reason: fmt.Sprintf("%s costs $%.2f/h, above your max of $%.2f/h.", gpu.Name, price, maxPrice)}
	}
	return gpu, nil
}

// Alternatives returns the GPUs available right now in the cloud, cheapest
// first, split by whether they fit within maxPrice. GPUs whose id is in
// exclude are left out, e.g. one RunPod just refused for lack of capacity.
func Alternatives(catalog []runpod.GPUType, cloud string, maxPrice float64, exclude []string) (affordable, tooExpensive []runpod.GPUType) {
	for _, gpu := range catalog {
		if slices.Contains(exclude, gpu.ID) || !offeredIn(gpu, cloud) || !available(gpu) {
			continue
		}
		if Price(gpu, cloud) <= maxPrice {
			affordable = append(affordable, gpu)
		} else {
			tooExpensive = append(tooExpensive, gpu)
		}
	}
	byPrice := func(gpus []runpod.GPUType) {
		sort.Slice(gpus, func(i, j int) bool { return Price(gpus[i], cloud) < Price(gpus[j], cloud) })
	}
	byPrice(affordable)
	byPrice(tooExpensive)
	return affordable, tooExpensive
}

// Price is the hourly price of one GPU of this type in the cloud.
func Price(gpu runpod.GPUType, cloud string) float64 {
	if cloud == "COMMUNITY" {
		return gpu.Price.Community
	}
	return gpu.Price.Secure
}

func offeredIn(gpu runpod.GPUType, cloud string) bool {
	if cloud == "COMMUNITY" {
		return gpu.Community
	}
	return gpu.Secure
}

func available(gpu runpod.GPUType) bool {
	return gpu.Availability != "NONE" && gpu.Availability != ""
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
	return FormatDuration(b.time)
}

// FormatDuration renders a duration as hours and minutes, e.g. "1h05".
func FormatDuration(d time.Duration) string {
	minutes := int(d.Round(time.Minute).Minutes())
	return fmt.Sprintf("%dh%02d", minutes/60, minutes%60)
}

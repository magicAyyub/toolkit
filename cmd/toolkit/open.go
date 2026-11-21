package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"toolkit/internal/runpod"
	"toolkit/internal/workshop"
)

func openWorkshop(ctx context.Context, config configFile, args []string) error {
	flags := flag.NewFlagSet("open", flag.ExitOnError)
	gpuName := flags.String("gpu", "RTX 4090", "GPU type, as named in the RunPod catalog")
	maxPrice := flags.Float64("max-price", 1.00, "maximum price in USD per hour")
	budgetFlag := flags.String("budget", "$3", `budget as a duration ("2h", "90m") or dollars ("$5")`)
	idle := flags.Duration("idle", 30*time.Minute, "delete after this long with no SSH session and GPU at rest")
	community := flags.Bool("community", false, "use the community cloud (cheaper, less reliable)")
	sshKeyFlag := flags.String("ssh-key", config.sshKey(), "SSH public key installed on the machine")
	yes := flags.Bool("yes", false, "never prompt: fail with suggestions instead, as when not on a terminal")
	debug := flags.Bool("debug", false, "show ssh probe commands, their output, and other diagnostics")
	name := parseWithName(flags, args)
	prompts := terminal && !*yes

	if err := workshop.ValidateName(name); err != nil {
		return err
	}
	budget, err := workshop.ParseBudget(*budgetFlag)
	if err != nil {
		return err
	}
	sshKey := expandHome(*sshKeyFlag)
	publicKey, err := os.ReadFile(sshKey)
	if err != nil {
		return fmt.Errorf("reading SSH public key: %w", err)
	}
	if err := workshop.CheckSSHKey(sshKey); err != nil {
		return err
	}
	client, err := config.newClient()
	if err != nil {
		return err
	}

	if _, err := workshop.Find(ctx, client, name); err == nil {
		return fmt.Errorf("workshop %q is already open. Reconnect with: toolkit workshop connect %s", name, name)
	} else if !errors.Is(err, workshop.ErrNotFound) {
		return err
	}

	cloud := "SECURE"
	if *community {
		cloud = "COMMUNITY"
	}
	catalog, err := client.ListGPUTypes(ctx, cloud)
	if err != nil {
		return fmt.Errorf("reading GPU catalog: %w", err)
	}
	gpu, err := workshop.MatchGPU(catalog, *gpuName, cloud, *maxPrice)
	var unavailable *workshop.UnavailableGPUError
	if errors.As(err, &unavailable) {
		gpu, err = chooseAlternative(unavailable.Reason, catalog, cloud, *maxPrice, nil, unavailable.Matches, prompts)
	}
	if err != nil {
		return err
	}

	spec := workshop.Spec{
		Name:        name,
		GPU:         gpu,
		Cloud:       cloud,
		Budget:      budget,
		IdleTimeout: *idle,
		PublicKey:   string(publicKey),
	}
	var refused []string // GPUs RunPod had no capacity for during this run
	for {
		progress := startProgress("Creating the pod on "+spec.GPU.Name, *debug)
		// Not cancellable: if Ctrl-C cut the request after RunPod accepted it,
		// we would lose track of a pod we must delete. Ctrl-C is honoured right
		// after, by WaitUntilReady, which deletes the pod.
		info, err := workshop.Create(context.WithoutCancel(ctx), client, spec)

		var capacity *workshop.CapacityError
		if errors.As(err, &capacity) {
			progress.Stop()
			refused = append(refused, spec.GPU.ID)
			catalog, err = client.ListGPUTypes(ctx, cloud)
			if err != nil {
				return fmt.Errorf("reading GPU catalog: %w", err)
			}
			reason := fmt.Sprintf("RunPod has no capacity for %s right now (%s).", spec.GPU.Name, capacity.Detail)
			spec.GPU, err = chooseAlternative(reason, catalog, cloud, *maxPrice, refused, nil, prompts)
			if err != nil {
				return err
			}
			continue
		}
		if err != nil {
			progress.Stop()
			return err
		}

		// The expected price is above --max-price when the user picked a GPU
		// above it in the list; only a price above what they agreed to is news.
		expected := max(*maxPrice, workshop.Price(spec.GPU, cloud))
		if info.Pod.Cost > expected {
			progress.Notice(fmt.Sprintf("RunPod reports $%.2f/h for this pod, above the expected $%.2f/h. Close it if that's not acceptable.", info.Pod.Cost, expected))
		}
		info, endpoint, err := workshop.WaitUntilReady(ctx, client, info, sshKey, progress.Report)
		progress.Stop()

		var abandoned *workshop.AbandonedError
		if errors.As(err, &abandoned) && abandoned.DeleteErr == nil {
			printError(abandoned.Cause)
			fmt.Fprintf(os.Stderr, "  Deleted pod %s, so nothing is left running (est. %s).\n", abandoned.PodID, formatCost(abandoned.EstimatedCost))
			return errReported
		}
		if err != nil {
			return err
		}

		printOpenSummary(info, spec.GPU, cloud)
		return runSession(ctx, client, info, endpoint, sshKey, *debug)
	}
}

// chooseAlternative is called when the requested GPU can't be used. On a
// terminal it lets the user pick another one; otherwise it prints the
// alternatives and fails.
//
// GPUs within maxPrice are offered first. Only when none is available does
// the picker show the cheapest ones above it, saying so in its title: picking
// one is the user's explicit decision to pay more. Without a terminal the max
// price is never exceeded. GPUs in exclude are left out, and restrictTo, when
// set, narrows the choice (to the matches of an ambiguous name).
func chooseAlternative(reason string, catalog []runpod.GPUType, cloud string, maxPrice float64, exclude []string, restrictTo []runpod.GPUType, prompts bool) (runpod.GPUType, error) {
	affordable, tooExpensive := workshop.Alternatives(catalog, cloud, maxPrice, exclude)
	if len(restrictTo) > 0 {
		affordable = keepOnly(affordable, restrictTo)
		tooExpensive = keepOnly(tooExpensive, restrictTo)
	}

	if prompts && (len(affordable) > 0 || len(tooExpensive) > 0) {
		title := fmt.Sprintf("%s\nPick a GPU within $%.2f/h:", reason, maxPrice)
		choices := affordable
		if len(affordable) == 0 {
			title = fmt.Sprintf("%s\nNothing available under $%.2f/h. Cheapest above:", reason, maxPrice)
			choices = tooExpensive[:min(3, len(tooExpensive))]
		}
		gpu, err := selectGPU(title, choices, cloud, maxPrice)
		if errors.Is(err, errCancelled) {
			fmt.Fprintln(os.Stderr, "Cancelled, nothing was created.")
			return runpod.GPUType{}, errReported
		}
		return gpu, err
	}

	printWarning("%s", reason)
	switch {
	case len(affordable) > 0:
		fmt.Fprintf(os.Stderr, "Available now within $%.2f/h:\n", maxPrice)
		printGPUSuggestions(affordable, cloud, 8)
	case len(tooExpensive) > 0:
		fmt.Fprintf(os.Stderr, "Nothing available within $%.2f/h. Cheapest available (raise --max-price):\n", maxPrice)
		printGPUSuggestions(tooExpensive, cloud, 3)
	default:
		fmt.Fprintf(os.Stderr, "No GPU is available in the %s cloud right now.\n", strings.ToLower(cloud))
	}
	return runpod.GPUType{}, errReported
}

func keepOnly(gpus, allowed []runpod.GPUType) []runpod.GPUType {
	var kept []runpod.GPUType
	for _, gpu := range gpus {
		if slices.ContainsFunc(allowed, func(a runpod.GPUType) bool { return a.ID == gpu.ID }) {
			kept = append(kept, gpu)
		}
	}
	return kept
}

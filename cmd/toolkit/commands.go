package main

import (
	"context"
	"fmt"
	"os"

	"toolkit/internal/runpod"
	"toolkit/internal/workshop"
)

func connectWorkshop(ctx context.Context, client *runpod.Client, name, sshKey string) error {
	info, err := workshop.Find(ctx, client, name)
	if err != nil {
		return err
	}
	progress := startProgress("Connecting to "+name, false)
	info, endpoint, err := workshop.Endpoint(ctx, client, info)
	progress.Stop()
	if err != nil {
		return err
	}
	return runSession(ctx, client, info, endpoint, sshKey, false)
}

// runSession runs the interactive SSH session, then tells the user what
// became of the workshop: still running, or deleted while they were on it.
func runSession(ctx context.Context, client *runpod.Client, info workshop.Info, endpoint runpod.SSHEndpoint, sshKey string, debug bool) error {
	connectionLost, err := workshop.RunSession(info, endpoint, sshKey, debug)
	if err != nil {
		return err
	}

	fmt.Println()
	phase := "Checking the workshop"
	if connectionLost {
		phase = "Connection lost, checking the workshop"
	}
	progress := startProgress(phase, debug)
	gone, checkErr := workshop.Gone(ctx, client, info, connectionLost)
	progress.Stop()

	if !gone {
		printDisconnectedSummary(info, checkErr)
		return nil
	}
	headline := fmt.Sprintf("The machine was deleted. Workshop %s is closed.", nameStyle.Render(info.Name))
	if info.BudgetReached() {
		headline = fmt.Sprintf("The machine was deleted (budget reached). Workshop %s is closed.", nameStyle.Render(info.Name))
	}
	printDeletedSummary(headline, info)
	return nil
}

func listWorkshops(ctx context.Context, client *runpod.Client) error {
	workshops, err := workshop.List(ctx, client)
	if err != nil {
		return err
	}
	if len(workshops) == 0 {
		fmt.Println("No open workshops.")
		return nil
	}

	// Pods only carry the GPU's full id ("NVIDIA GeForce RTX 4090"); the
	// catalog has the short name. Without it, the id is shown.
	gpuNames := map[string]string{}
	if catalog, err := client.ListGPUTypes(ctx, "SECURE"); err == nil {
		for _, gpu := range catalog {
			gpuNames[gpu.ID] = gpu.Name
		}
	}
	printWorkshops(workshops, gpuNames)
	return nil
}

func closeWorkshop(ctx context.Context, client *runpod.Client, name string, yes bool) error {
	info, err := workshop.Find(ctx, client, name)
	if err != nil {
		return err
	}

	if !yes {
		if !terminal {
			return fmt.Errorf("not deleting workshop %q without confirmation: run it with --yes", name)
		}
		confirmed, err := confirmDelete(info)
		if err != nil && err != errCancelled {
			return err
		}
		if !confirmed {
			fmt.Fprintln(os.Stderr, "Cancelled, nothing was deleted.")
			return nil
		}
	}

	progress := startProgress("Deleting "+name, false)
	err = workshop.Delete(ctx, client, info)
	progress.Stop()
	if err != nil {
		return err
	}
	printDeletedSummary(fmt.Sprintf("Workshop %s deleted.", nameStyle.Render(name)), info)
	return nil
}

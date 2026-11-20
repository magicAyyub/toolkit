package workshop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"toolkit/internal/runpod"
)

const (
	sshWaitTimeout = 10 * time.Minute
	// Failed probes are normal while sshd starts; only report the ssh error
	// once it keeps failing.
	probeFailuresBeforeReport = 3
)

// checkSSHKey makes sure ssh will be able to use the key without prompting,
// before we pay for a pod: either the private key has no passphrase, or the
// key is loaded in ssh-agent.
func checkSSHKey(publicKeyPath string) error {
	privateKeyPath := strings.TrimSuffix(publicKeyPath, ".pub")
	if _, err := os.Stat(privateKeyPath); err != nil {
		return fmt.Errorf("SSH private key %s not found (expected next to %s)", privateKeyPath, publicKeyPath)
	}

	if exec.Command("ssh-keygen", "-y", "-P", "", "-f", privateKeyPath).Run() == nil {
		return nil
	}

	publicKey, err := os.ReadFile(publicKeyPath)
	if err != nil {
		return fmt.Errorf("reading SSH public key: %w", err)
	}
	// Compare "type base64" and ignore the comment, which can differ.
	keyFields := strings.Fields(string(publicKey))
	if len(keyFields) < 2 {
		return fmt.Errorf("%s does not look like an SSH public key", publicKeyPath)
	}
	agentKeys, _ := exec.Command("ssh-add", "-L").Output()
	for _, line := range strings.Split(string(agentKeys), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == keyFields[0] && fields[1] == keyFields[1] {
			return nil
		}
	}
	return fmt.Errorf("SSH key %s needs a passphrase and is not loaded in ssh-agent.\nLoad it first with: ssh-add %s", privateKeyPath, privateKeyPath)
}

// waitForSSH polls the pod until it is running with a public SSH port, then
// until sshd actually accepts our key (the image sets it up after boot). It
// gives up early when RunPod reports that the pod can't start.
func waitForSSH(ctx context.Context, client *runpod.Client, podID, sshKey string, debug bool) (runpod.Pod, runpod.SSHEndpoint, error) {
	ctx, cancel := context.WithTimeout(ctx, sshWaitTimeout)
	defer cancel()

	// Host-side failures, such as an image that can't be pulled, only show up
	// in RunPod's system log: the pod status can stay PROVISIONING meanwhile.
	startupErrors := make(chan string, 1)
	go func() {
		err := client.StreamPodLogs(ctx, podID, "system", func(line string) {
			if strings.Contains(strings.ToLower(line), "error") {
				select {
				case startupErrors <- line:
				default:
				}
			}
		})
		if err != nil && ctx.Err() == nil {
			fmt.Printf("\n  (can't read RunPod system logs, watching the pod status only: %v)\n  still waiting", err)
		}
	}()

	status := "not reported yet"
	probing := false
	probeFailures := 0
	lastProbeError := ""
	lastReportedError := ""
	for {
		// API errors are ignored here: transient failures are expected while a
		// pod comes up, and the timeout bounds the wait anyway.
		pod, err := client.GetPod(ctx, podID)
		if err == nil {
			status = pod.Status
			switch status {
			case "ERROR":
				return runpod.Pod{}, runpod.SSHEndpoint{}, errors.New("RunPod reports the container in an unrecoverable ERROR state")
			case "EXITED", "TERMINATED":
				return runpod.Pod{}, runpod.SSHEndpoint{}, fmt.Errorf("the pod stopped while starting (status %s)", status)
			}
			if status == "RUNNING" && pod.SSH.Direct != nil {
				endpoint := *pod.SSH.Direct
				if !probing {
					fmt.Printf(" on %s ", endpointAddress(endpoint))
					probing = true
				}
				probeError := probeSSH(endpoint, sshKey, debug)
				if probeError == "" {
					return pod, endpoint, nil
				}
				probeFailures++
				lastProbeError = probeError
				if probeFailures >= probeFailuresBeforeReport && probeError != lastReportedError {
					fmt.Printf("\n  ssh %s: %s\n  still waiting", endpointAddress(endpoint), probeError)
					lastReportedError = probeError
				}
			}
		}
		fmt.Print(".")

		select {
		case line := <-startupErrors:
			return runpod.Pod{}, runpod.SSHEndpoint{}, fmt.Errorf("RunPod reported an error while starting the pod: %s", line)
		case <-ctx.Done():
			if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return runpod.Pod{}, runpod.SSHEndpoint{}, errors.New("interrupted while waiting for SSH")
			}
			if lastProbeError != "" {
				return runpod.Pod{}, runpod.SSHEndpoint{}, fmt.Errorf("SSH still not reachable after %s: %s", sshWaitTimeout, lastProbeError)
			}
			return runpod.Pod{}, runpod.SSHEndpoint{}, fmt.Errorf("the pod never became reachable: status still %s after %s", status, sshWaitTimeout)
		case <-time.After(5 * time.Second):
		}
	}
}

// probeSSH runs a no-op command and returns ssh's error output, or "" on success.
//
// The probe doesn't check or record the host key, so nothing seen while the
// pod boots ends up in its known_hosts file. It only runs "true", so there is
// nothing to protect; the interactive session does the trust-on-first-use check.
func probeSSH(endpoint runpod.SSHEndpoint, sshKey string, debug bool) string {
	options := []string{
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "StrictHostKeyChecking=no",
		"-o", "LogLevel=ERROR",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=5",
	}
	probe := sshCommand(endpoint, sshKey, options, "true")
	started := time.Now()
	output, err := probe.CombinedOutput()
	message := strings.TrimSpace(string(output))

	if debug {
		exitCode := 0
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else if err != nil {
			exitCode = -1 // ssh could not be started at all
		}
		fmt.Printf("\n[debug] $ %s\n[debug] exit %d after %s", shellQuote(probe.Args), exitCode, time.Since(started).Round(time.Millisecond))
		if message != "" {
			fmt.Printf(", output:\n%s", message)
		}
		if err != nil && exitCode == -1 {
			fmt.Printf(", error: %v", err)
		}
		fmt.Println()
	}

	if err == nil {
		return ""
	}
	if message != "" {
		return message
	}
	return err.Error()
}

// shellQuote renders a command so it can be pasted into a shell as is.
func shellQuote(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		if arg != "" && strings.Trim(arg, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789@%+=:,./_-") == "" {
			quoted[i] = arg
		} else {
			quoted[i] = "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
		}
	}
	return strings.Join(quoted, " ")
}

func runInteractiveSSH(ctx context.Context, client *runpod.Client, pod runpod.Pod, endpoint runpod.SSHEndpoint, sshKey string) error {
	options := []string{
		// Pods reuse public IP:port pairs with fresh host keys, so the user's main
		// known_hosts would raise mismatch errors. Each pod gets its own file,
		// trusted on first use.
		"-o", "UserKnownHostsFile=" + knownHostsFile(pod.ID),
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ServerAliveInterval=30",
	}
	cmd := sshCommand(endpoint, sshKey, options)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()

	// A non-zero exit is usually just the remote shell's last status, or a
	// dropped connection (255). Neither is an error for the toolkit.
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return fmt.Errorf("running ssh: %w", err)
	}
	connectionLost := exitErr != nil && exitErr.ExitCode() == 255

	name := strings.TrimPrefix(pod.Name, podNamePrefix)
	fmt.Println()
	if connectionLost {
		fmt.Println("Connection lost, checking the workshop...")
	}
	gone, checkErr := podGone(ctx, client, pod.ID, connectionLost)
	if gone {
		forgetHostKey(pod.ID)
		info := readWorkshopEnv(pod)
		reason := ""
		if !info.deadline.IsZero() && !time.Now().Before(info.deadline) {
			reason = " (budget reached)"
		}
		duration := time.Since(info.created)
		fmt.Printf("The machine was deleted%s. Workshop %q is closed.\n", reason, name)
		fmt.Printf("  Duration   %s\n", formatDuration(duration))
		fmt.Printf("  Est. cost  ~$%.2f ($%.2f/h)\n", pod.Cost*duration.Hours(), pod.Cost)
		return nil
	}

	if checkErr != nil {
		fmt.Printf("Disconnected from workshop %q (could not check whether it still exists: %v).\n", name, checkErr)
	} else {
		fmt.Printf("Disconnected from workshop %q. It keeps running.\n", name)
	}
	fmt.Printf("  Reconnect  toolkit workshop connect %s\n", name)
	fmt.Printf("  Close      toolkit workshop close %s\n", name)
	if idle, err := strconv.Atoi(pod.Env[envIdleSeconds]); err == nil {
		fmt.Printf("  If nothing runs on the GPU, it will be deleted after %s idle.\n", formatDuration(time.Duration(idle)*time.Second))
	}
	return nil
}

// podGone reports whether the pod no longer exists. After a lost connection
// it checks a few times, because the API can briefly lag behind a deletion
// the guardian has just made.
func podGone(ctx context.Context, client *runpod.Client, podID string, connectionLost bool) (bool, error) {
	attempts := 1
	if connectionLost {
		attempts = 4
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		pod, err := client.GetPod(ctx, podID)
		var apiErr *runpod.APIError
		switch {
		case errors.As(err, &apiErr) && apiErr.Status == 404:
			return true, nil
		case err != nil:
			lastErr = err
		case pod.Status == "TERMINATED":
			return true, nil
		default:
			lastErr = nil
		}
		if attempt < attempts {
			time.Sleep(podGoneRetryDelay)
		}
	}
	return false, lastErr
}

var podGoneRetryDelay = 3 * time.Second // shortened by tests

// sshCommand builds "ssh <options> user@host <remote command>". ssh takes
// everything after the destination as the remote command, so options must
// come before it.
func sshCommand(endpoint runpod.SSHEndpoint, sshKey string, options []string, remoteCommand ...string) *exec.Cmd {
	args := []string{"-p", strconv.Itoa(endpoint.Port), "-i", strings.TrimSuffix(sshKey, ".pub")}
	args = append(args, options...)
	args = append(args, sshUser(endpoint)+"@"+endpoint.Host)
	args = append(args, remoteCommand...)
	return exec.Command("ssh", args...)
}

func sshUser(endpoint runpod.SSHEndpoint) string {
	if endpoint.Username == "" {
		return "root"
	}
	return endpoint.Username
}

func endpointAddress(endpoint runpod.SSHEndpoint) string {
	return fmt.Sprintf("%s@%s:%d", sshUser(endpoint), endpoint.Host, endpoint.Port)
}

func knownHostsFile(podID string) string {
	dir := filepath.Join(ConfigDir(), "known_hosts")
	os.MkdirAll(dir, 0o700)
	return filepath.Join(dir, podID)
}

func forgetHostKey(podID string) {
	os.Remove(knownHostsFile(podID))
}

// ConfigDir is where the toolkit keeps its config and per-pod known_hosts files.
func ConfigDir() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "toolkit")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "toolkit")
}

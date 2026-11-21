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
	// RunPod can briefly report no SSH port for a running pod, e.g. right
	// after a session dropped.
	endpointWaitTimeout = 30 * time.Second
)

// Shortened by tests.
var (
	pollInterval      = 5 * time.Second
	podGoneRetryDelay = 3 * time.Second
)

// CheckSSHKey makes sure ssh will be able to use the key without prompting,
// before we pay for a pod: either the private key has no passphrase, or the
// key is loaded in ssh-agent.
func CheckSSHKey(publicKeyPath string) error {
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

// Waiting for a new pod

// Event reports progress while waiting for a pod. Exactly one field is set.
type Event struct {
	Phase  string // what is happening now, e.g. "Starting the machine"
	Notice string // something the user should see, e.g. a repeated ssh error
	Debug  string // details only useful with --debug
}

// AbandonedError means WaitUntilReady gave up. Since it can't tell whether the
// guardian ever started, it deleted the pod rather than leave it unprotected.
type AbandonedError struct {
	Cause         error
	PodID         string
	EstimatedCost float64
	DeleteErr     error // set if the pod could not be deleted and may still cost money
}

func (e *AbandonedError) Error() string {
	if e.DeleteErr != nil {
		return fmt.Sprintf("%v\ncould not delete pod %s (%v). It may keep costing money: delete it from the RunPod console", e.Cause, e.PodID, e.DeleteErr)
	}
	return e.Cause.Error()
}

func (e *AbandonedError) Unwrap() error {
	return e.Cause
}

// WaitUntilReady waits until the new pod is running and sshd accepts our key.
// It gives up early when RunPod reports that the pod can't start, after
// sshWaitTimeout, or when ctx is cancelled (Ctrl-C), and then deletes the pod:
// the error is an *AbandonedError.
func WaitUntilReady(ctx context.Context, client *runpod.Client, info Info, sshKey string, report func(Event)) (Info, runpod.SSHEndpoint, error) {
	pod, endpoint, err := waitForSSH(ctx, client, info.Pod.ID, sshKey, report)
	if err == nil {
		info.Pod = pod
		return info, endpoint, nil
	}

	// A fresh context: ctx may be the one Ctrl-C just cancelled.
	deleteCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return info, runpod.SSHEndpoint{}, &AbandonedError{
		Cause:         err,
		PodID:         info.Pod.ID,
		EstimatedCost: info.EstimatedCost(),
		DeleteErr:     Delete(deleteCtx, client, info),
	}
}

func waitForSSH(ctx context.Context, client *runpod.Client, podID, sshKey string, report func(Event)) (runpod.Pod, runpod.SSHEndpoint, error) {
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
			report(Event{Debug: fmt.Sprintf("can't read RunPod system logs, watching the pod status only: %v", err)})
		}
	}()

	report(Event{Phase: "Starting the machine"})
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
					report(Event{Phase: "Waiting for SSH on " + endpointAddress(endpoint)})
					probing = true
				}
				probe := probeSSH(endpoint, sshKey)
				report(Event{Debug: probe.debugText()})
				if probe.ok() {
					return pod, endpoint, nil
				}
				probeFailures++
				lastProbeError = probe.failure()
				if probeFailures >= probeFailuresBeforeReport && lastProbeError != lastReportedError {
					report(Event{Notice: fmt.Sprintf("ssh %s: %s", endpointAddress(endpoint), lastProbeError)})
					lastReportedError = lastProbeError
				}
			}
		}

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
		case <-time.After(pollInterval):
		}
	}
}

type probeResult struct {
	command  string
	exitCode int // -1 when ssh could not be started at all
	duration time.Duration
	output   string
	err      error
}

// probeSSH runs a no-op command over SSH.
//
// The probe doesn't check or record the host key, so nothing seen while the
// pod boots ends up in its known_hosts file. It only runs "true", so there is
// nothing to protect; the interactive session does the trust-on-first-use check.
func probeSSH(endpoint runpod.SSHEndpoint, sshKey string) probeResult {
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

	result := probeResult{
		command:  shellQuote(probe.Args),
		duration: time.Since(started),
		output:   strings.TrimSpace(string(output)),
		err:      err,
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.exitCode = exitErr.ExitCode()
	} else if err != nil {
		result.exitCode = -1
	}
	return result
}

func (r probeResult) ok() bool {
	return r.err == nil
}

// failure is what went wrong, preferably in ssh's own words.
func (r probeResult) failure() string {
	if r.output != "" {
		return r.output
	}
	return r.err.Error()
}

func (r probeResult) debugText() string {
	text := fmt.Sprintf("$ %s\nexit %d after %s", r.command, r.exitCode, r.duration.Round(time.Millisecond))
	if r.output != "" {
		text += ", output:\n" + r.output
	}
	if r.exitCode == -1 {
		text += fmt.Sprintf(", error: %v", r.err)
	}
	return text
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

// Connecting to an existing workshop

// Endpoint returns the workshop's direct SSH endpoint, waiting up to
// endpointWaitTimeout while RunPod reports none yet.
func Endpoint(ctx context.Context, client *runpod.Client, info Info) (Info, runpod.SSHEndpoint, error) {
	ctx, cancel := context.WithTimeout(ctx, endpointWaitTimeout)
	defer cancel()

	for {
		// List results don't include live connection details; the single pod does.
		pod, err := client.GetPod(ctx, info.Pod.ID)
		if err == nil {
			switch {
			case pod.Status == "RUNNING" && pod.SSH.Direct != nil:
				info.Pod = pod
				return info, *pod.SSH.Direct, nil
			case pod.Status == "EXITED" || pod.Status == "ERROR" || pod.Status == "TERMINATED":
				return info, runpod.SSHEndpoint{}, fmt.Errorf("workshop %q is %s, not RUNNING", info.Name, pod.Status)
			}
		}

		select {
		case <-ctx.Done():
			if err != nil {
				return info, runpod.SSHEndpoint{}, fmt.Errorf("reading workshop %q: %w", info.Name, err)
			}
			return info, runpod.SSHEndpoint{}, fmt.Errorf("workshop %q has no public SSH port after %s (status %s)", info.Name, endpointWaitTimeout, pod.Status)
		case <-time.After(pollInterval / 2):
		}
	}
}

// Interactive session

// RunSession connects the terminal to the workshop over SSH until the user
// leaves or the connection drops; connectionLost tells which.
func RunSession(info Info, endpoint runpod.SSHEndpoint, sshKey string, debug bool) (connectionLost bool, err error) {
	options := []string{
		// Pods reuse public IP:port pairs with fresh host keys, so the user's main
		// known_hosts would raise mismatch errors. Each pod gets its own file,
		// trusted on first use.
		"-o", "UserKnownHostsFile=" + knownHostsFile(info.Pod.ID),
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ServerAliveInterval=30",
	}
	if !debug {
		// Hides "Permanently added ... to the list of known hosts" on first connect.
		options = append(options, "-o", "LogLevel=ERROR")
	}
	cmd := sshCommand(endpoint, sshKey, options)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Run()

	// A non-zero exit is usually just the remote shell's last status, or a
	// dropped connection (255). Neither is an error for the toolkit.
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return false, fmt.Errorf("running ssh: %w", err)
	}
	return exitErr != nil && exitErr.ExitCode() == 255, nil
}

// Gone reports whether the workshop's pod no longer exists. After a lost
// connection it checks a few times, because the API can briefly lag behind a
// deletion the guardian has just made.
func Gone(ctx context.Context, client *runpod.Client, info Info, connectionLost bool) (bool, error) {
	attempts := 1
	if connectionLost {
		attempts = 4
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		pod, err := client.GetPod(ctx, info.Pod.ID)
		var apiErr *runpod.APIError
		switch {
		case errors.As(err, &apiErr) && apiErr.Status == 404:
			forgetHostKey(info.Pod.ID)
			return true, nil
		case err != nil:
			lastErr = err
		case pod.Status == "TERMINATED":
			forgetHostKey(info.Pod.ID)
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

// SSH plumbing

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

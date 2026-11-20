package workshop

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"toolkit/internal/runpod"
)

func TestCheckSSHKey(t *testing.T) {
	dir := t.TempDir()
	generateKey := func(name, passphrase string) string {
		path := filepath.Join(dir, name)
		output, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", passphrase, "-C", "test", "-f", path).CombinedOutput()
		if err != nil {
			t.Fatalf("ssh-keygen: %v\n%s", err, output)
		}
		return path + ".pub"
	}
	plainKey := generateKey("plain", "")
	protectedKey := generateKey("protected", "secret passphrase")

	// A private agent, so the test neither depends on nor touches the user's.
	agentSocket := filepath.Join(dir, "agent.sock")
	agentOutput, err := exec.Command("ssh-agent", "-a", agentSocket).Output()
	if err != nil {
		t.Fatalf("starting ssh-agent: %v", err)
	}
	agentPID := regexp.MustCompile(`SSH_AGENT_PID=(\d+)`).FindStringSubmatch(string(agentOutput))
	if agentPID == nil {
		t.Fatalf("unexpected ssh-agent output: %s", agentOutput)
	}
	t.Cleanup(func() { exec.Command("kill", agentPID[1]).Run() })
	t.Setenv("SSH_AUTH_SOCK", agentSocket)

	if err := checkSSHKey(plainKey); err != nil {
		t.Errorf("key without passphrase: %v", err)
	}

	err = checkSSHKey(protectedKey)
	if err == nil || !strings.Contains(err.Error(), "needs a passphrase and is not loaded in ssh-agent") {
		t.Errorf("protected key not in agent: got %v", err)
	}

	// Load the protected key through SSH_ASKPASS, since ssh-add has no passphrase flag.
	askpass := filepath.Join(dir, "askpass")
	os.WriteFile(askpass, []byte("#!/bin/sh\necho 'secret passphrase'\n"), 0o755)
	add := exec.Command("ssh-add", strings.TrimSuffix(protectedKey, ".pub"))
	add.Env = append(os.Environ(), "SSH_ASKPASS="+askpass, "SSH_ASKPASS_REQUIRE=force", "DISPLAY=none")
	if output, err := add.CombinedOutput(); err != nil {
		t.Fatalf("ssh-add: %v\n%s", err, output)
	}
	if err := checkSSHKey(protectedKey); err != nil {
		t.Errorf("protected key loaded in agent: %v", err)
	}

	if err := checkSSHKey(filepath.Join(dir, "missing.pub")); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("missing key: got %v", err)
	}
}

func TestSSHCommandPutsRemoteCommandAfterDestination(t *testing.T) {
	endpoint := runpod.SSHEndpoint{Host: "203.0.113.7", Port: 2222, Username: "root"}
	cmd := sshCommand(endpoint, "/keys/id.pub", []string{"-o", "BatchMode=yes"}, "true")
	want := "ssh -p 2222 -i /keys/id -o BatchMode=yes root@203.0.113.7 true"
	if got := strings.Join(cmd.Args, " "); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// TestProbeSSHAgainstRealServer runs the probe against a throwaway sshd on a
// local port, as the current user.
func TestProbeSSHAgainstRealServer(t *testing.T) {
	sshdPath, err := exec.LookPath("sshd")
	if err != nil {
		sshdPath = "/usr/sbin/sshd"
	}
	if _, err := os.Stat(sshdPath); err != nil {
		t.Skip("sshd not available")
	}

	dir := t.TempDir()
	keygen := func(path string) {
		if output, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", path).CombinedOutput(); err != nil {
			t.Fatalf("ssh-keygen: %v\n%s", err, output)
		}
	}
	keygen(filepath.Join(dir, "host_key"))
	keygen(filepath.Join(dir, "client_key"))
	clientPublicKey, _ := os.ReadFile(filepath.Join(dir, "client_key.pub"))
	os.WriteFile(filepath.Join(dir, "authorized_keys"), clientPublicKey, 0o600)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen locally: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	config := fmt.Sprintf("Port %d\nListenAddress 127.0.0.1\nHostKey %s\nAuthorizedKeysFile %s\nStrictModes no\nPasswordAuthentication no\nKbdInteractiveAuthentication no\nUsePAM no\nPidFile %s\n",
		port, filepath.Join(dir, "host_key"), filepath.Join(dir, "authorized_keys"), filepath.Join(dir, "sshd.pid"))
	os.WriteFile(filepath.Join(dir, "sshd_config"), []byte(config), 0o600)

	sshd := exec.Command(sshdPath, "-D", "-e", "-f", filepath.Join(dir, "sshd_config"))
	var sshdLog strings.Builder
	sshd.Stderr = &sshdLog
	if err := sshd.Start(); err != nil {
		t.Skipf("cannot start sshd: %v", err)
	}
	t.Cleanup(func() { sshd.Process.Kill(); sshd.Wait() })

	current, _ := user.Current()
	endpoint := runpod.SSHEndpoint{Host: "127.0.0.1", Port: port, Username: current.Username}

	probeError := "not tried"
	for attempt := 0; attempt < 20 && probeError != ""; attempt++ {
		probeError = probeSSH(endpoint, filepath.Join(dir, "client_key.pub"), false)
		if probeError != "" {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if probeError != "" {
		t.Fatalf("probe against a working sshd failed: %s\nsshd log:\n%s", probeError, sshdLog.String())
	}

	// A key the server doesn't know must fail, and the error must say why.
	keygen(filepath.Join(dir, "unknown_key"))
	t.Setenv("SSH_AUTH_SOCK", "") // keep the user's agent keys out of it
	if message := probeSSH(endpoint, filepath.Join(dir, "unknown_key.pub"), false); !strings.Contains(message, "Permission denied") {
		t.Errorf("expected a permission error with an unknown key, got %q", message)
	}
}

func TestPodGone(t *testing.T) {
	defaultDelay := podGoneRetryDelay
	podGoneRetryDelay = time.Millisecond
	t.Cleanup(func() { podGoneRetryDelay = defaultDelay })
	cases := []struct {
		name           string
		responses      []int // HTTP status per GET, the last one repeats; 200 means RUNNING
		connectionLost bool
		wantGone       bool
		wantErr        bool
		wantCalls      int
	}{
		{"deleted", []int{404}, false, true, false, 1},
		{"still running after a normal exit", []int{200}, false, false, false, 1},
		{"deletion visible after a lost connection", []int{200, 200, 404}, true, true, false, 3},
		{"still running after a lost connection", []int{200}, true, false, false, 4},
		{"API unreachable", []int{500}, false, false, true, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			calls := 0
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				status := c.responses[min(calls, len(c.responses)-1)]
				calls++
				if status != 200 {
					http.Error(w, `{"title":"nope"}`, status)
					return
				}
				fmt.Fprint(w, `{"id":"pod-1","status":"RUNNING"}`)
			}))
			defer api.Close()
			client := runpod.NewClient("test-key")
			client.BaseURL = api.URL

			gone, err := podGone(t.Context(), client, "pod-1", c.connectionLost)
			if gone != c.wantGone || (err != nil) != c.wantErr || calls != c.wantCalls {
				t.Errorf("got gone=%v err=%v after %d calls; want gone=%v err=%v after %d calls",
					gone, err, calls, c.wantGone, c.wantErr, c.wantCalls)
			}
		})
	}
}

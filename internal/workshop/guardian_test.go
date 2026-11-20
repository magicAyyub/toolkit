package workshop

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// These tests run the real guardian script with fake nvidia-smi and pgrep,
// a fake RunPod API, and a 1 second poll interval.

type guardianScenario struct {
	sshSessions    int
	gpuUtilization string // nvidia-smi output; empty means nvidia-smi fails
	budget         time.Duration
	idleSeconds    int
}

type guardianRun struct {
	deletes atomic.Int32
	cmd     *exec.Cmd
	exited  chan error
	ttyFile string
	logFile string
}

func startGuardian(t *testing.T, scenario guardianScenario) *guardianRun {
	t.Helper()
	dir := t.TempDir()
	run := &guardianRun{exited: make(chan error, 1)}

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v2/pods/pod-123" || r.Header.Get("Authorization") != "Bearer pod-scoped-key" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		run.deletes.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(api.Close)

	bin := filepath.Join(dir, "bin")
	os.Mkdir(bin, 0o755)
	writeExecutable(t, filepath.Join(bin, "pgrep"), "#!/bin/sh\necho "+strconv.Itoa(scenario.sshSessions)+"\n")
	nvidiaSmi := "#!/bin/sh\nexit 1\n"
	if scenario.gpuUtilization != "" {
		nvidiaSmi = "#!/bin/sh\nprintf '" + scenario.gpuUtilization + "\\n'\n"
	}
	writeExecutable(t, filepath.Join(bin, "nvidia-smi"), nvidiaSmi)

	ttyDir := filepath.Join(dir, "pts")
	os.Mkdir(ttyDir, 0o755)
	run.ttyFile = filepath.Join(ttyDir, "0")
	os.WriteFile(run.ttyFile, nil, 0o644)

	now := time.Now()
	run.cmd = exec.Command("bash", "guardian.sh")
	run.cmd.Env = []string{
		"PATH=" + bin + ":" + os.Getenv("PATH"),
		"HOME=" + dir,
		"RUNPOD_API_KEY=pod-scoped-key",
		"RUNPOD_POD_ID=pod-123",
		"WS_API_URL=" + api.URL,
		"WS_POLL_SECONDS=1",
		"WS_STATE_DIR=" + filepath.Join(dir, "state"),
		"WS_TTY_DIR=" + ttyDir,
		"WS_NAME=test",
		"WS_CREATED=" + strconv.FormatInt(now.Unix(), 10),
		"WS_DEADLINE=" + strconv.FormatInt(now.Add(scenario.budget).Unix(), 10),
		"WS_PRICE_PER_HOUR=0.44",
		"WS_BUDGET=test",
		"WS_IDLE_SECONDS=" + strconv.Itoa(scenario.idleSeconds),
	}
	run.logFile = filepath.Join(dir, "guardian.log")
	logOutput, _ := os.Create(run.logFile)
	run.cmd.Stdout = logOutput
	run.cmd.Stderr = logOutput

	if err := run.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { run.exited <- run.cmd.Wait() }()
	t.Cleanup(func() { run.cmd.Process.Kill() })
	return run
}

func (run *guardianRun) waitForExit(t *testing.T, timeout time.Duration) bool {
	t.Helper()
	select {
	case <-run.exited:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (run *guardianRun) log() string {
	content, _ := os.ReadFile(run.logFile)
	return string(content)
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestGuardianDeletesWhenBudgetIsReached(t *testing.T) {
	t.Parallel()
	run := startGuardian(t, guardianScenario{sshSessions: 1, gpuUtilization: "90", budget: 2 * time.Second, idleSeconds: 3600})

	if !run.waitForExit(t, 10*time.Second) {
		t.Fatalf("guardian did not delete the pod when the budget ran out\n%s", run.log())
	}
	if run.deletes.Load() != 1 {
		t.Errorf("expected 1 delete, got %d", run.deletes.Load())
	}
	tty, _ := os.ReadFile(run.ttyFile)
	if !strings.Contains(string(tty), "will be DELETED in 1 min") || !strings.Contains(string(tty), "Budget reached") {
		t.Errorf("expected warning then final notice on the terminal, got:\n%s", tty)
	}
}

func TestGuardianDeletesWhenIdle(t *testing.T) {
	t.Parallel()
	run := startGuardian(t, guardianScenario{sshSessions: 0, gpuUtilization: "0\n2", budget: time.Hour, idleSeconds: 2})

	if !run.waitForExit(t, 10*time.Second) {
		t.Fatalf("guardian did not delete an idle pod\n%s", run.log())
	}
	if run.deletes.Load() != 1 {
		t.Errorf("expected 1 delete, got %d", run.deletes.Load())
	}
}

func TestGuardianKeepsPodAlive(t *testing.T) {
	t.Parallel()
	cases := map[string]guardianScenario{
		"GPU job running after disconnect": {sshSessions: 0, gpuUtilization: "0\n85"},
		"SSH session open, GPU at rest":    {sshSessions: 1, gpuUtilization: "0"},
		"GPU utilization unreadable":       {sshSessions: 0, gpuUtilization: ""},
		"GPU reports N/A":                  {sshSessions: 0, gpuUtilization: "[N/A]"},
	}
	for name, scenario := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			scenario.budget = time.Hour
			scenario.idleSeconds = 1
			run := startGuardian(t, scenario)

			if run.waitForExit(t, 4*time.Second) {
				t.Fatalf("guardian exited\n%s", run.log())
			}
			if run.deletes.Load() != 0 {
				t.Errorf("pod was deleted\n%s", run.log())
			}
		})
	}
}

func TestGuardianInstallsPrompt(t *testing.T) {
	t.Parallel()
	run := startGuardian(t, guardianScenario{sshSessions: 1, gpuUtilization: "50", budget: time.Hour, idleSeconds: 3600})
	time.Sleep(1500 * time.Millisecond)

	home := filepath.Dir(filepath.Dir(run.ttyFile))
	env, err := os.ReadFile(filepath.Join(home, "state", "workshop.env"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(env), "pod-scoped-key") {
		t.Error("the API key must not be written to the session environment file")
	}
	if !strings.Contains(string(env), "WS_PRICE_PER_HOUR=0.44") {
		t.Errorf("unexpected workshop.env:\n%s", env)
	}
	bashrc, _ := os.ReadFile(filepath.Join(home, ".bashrc"))
	if strings.Count(string(bashrc), "\n") != 1 || !strings.Contains(string(bashrc), "prompt.sh") {
		t.Errorf("expected one prompt line in .bashrc, got:\n%s", bashrc)
	}

	// The prompt itself must work in a fresh interactive shell.
	summary := exec.Command("bash", "-c", ". "+filepath.Join(home, "state", "prompt.sh")+" && __ws_summary")
	output, err := summary.CombinedOutput()
	if err != nil || !strings.HasPrefix(string(output), "0h00/1h00 | ~$0.00/$0.44 est.") {
		t.Errorf("unexpected prompt summary %q (err %v)", output, err)
	}
}

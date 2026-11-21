package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// These tests drive the commands without a terminal, the way a script would:
// no prompt may ever show up, and the output must stay readable.

type fakeAPI struct {
	catalog      []map[string]any
	createStatus int // 0 means 201
	pods         []map[string]any
	creates      atomic.Int32
	deletes      atomic.Int32
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v2/catalog/gpus":
		json.NewEncoder(w).Encode(map[string]any{"gpus": f.catalog})
	case r.Method == http.MethodGet && r.URL.Path == "/v2/pods":
		json.NewEncoder(w).Encode(map[string]any{"pods": f.pods, "pagination": map[string]any{"hasNextPage": false}})
	case r.Method == http.MethodPost && r.URL.Path == "/v2/pods":
		f.creates.Add(1)
		if f.createStatus != 0 {
			w.WriteHeader(f.createStatus)
			fmt.Fprint(w, `{"title":"Bad Request","detail":"There are no longer any instances available with the requested specifications."}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":"pod-1","name":"ws-demo","status":"PROVISIONING","cost":0.25}`)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v2/pods/"):
		f.deletes.Add(1)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unexpected request "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}
}

func gpu(id, name string, memory int, price float64, availability string) map[string]any {
	return map[string]any{
		"id": id, "name": name, "memory": memory, "secure": true, "community": true,
		"price": map[string]any{"secure": price, "community": price}, "availability": availability,
	}
}

var testCatalog = []map[string]any{
	gpu("NVIDIA GeForce RTX 4090", "RTX 4090", 24, 0.69, "NONE"),
	gpu("NVIDIA RTX A4000", "RTX A4000", 16, 0.25, "HIGH"),
	gpu("NVIDIA RTX A5000", "RTX A5000", 24, 0.29, "LOW"),
	gpu("NVIDIA A100 80GB PCIe", "A100 PCIe", 80, 1.64, "HIGH"),
}

func startFakeAPI(t *testing.T, api *fakeAPI) configFile {
	t.Helper()
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	t.Setenv("RUNPOD_API_KEY", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	terminal = false
	return configFile{settings: map[string]string{"runpod_api_key": "test-key"}, apiBaseURL: server.URL}
}

func testSSHKey(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "key")
	if output, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", path).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, output)
	}
	return path + ".pub"
}

// captureOutput runs fn and returns what it wrote to stdout and stderr.
func captureOutput(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	read := func(target **os.File) func() string {
		reader, writer, _ := os.Pipe()
		original := *target
		*target = writer
		result := make(chan string)
		go func() {
			content, _ := io.ReadAll(reader)
			result <- string(content)
		}()
		return func() string {
			*target = original
			writer.Close()
			return <-result
		}
	}
	finishStdout := read(&os.Stdout)
	finishStderr := read(&os.Stderr)
	fn()
	return finishStdout(), finishStderr()
}

func TestOpenWithoutTerminalSuggestsAlternatives(t *testing.T) {
	api := &fakeAPI{catalog: testCatalog}
	config := startFakeAPI(t, api)
	key := testSSHKey(t)

	var err error
	_, stderr := captureOutput(t, func() {
		err = openWorkshop(context.Background(), config, []string{"demo", "--gpu", "RTX 4090", "--ssh-key", key})
	})

	if !errors.Is(err, errReported) || api.creates.Load() != 0 {
		t.Fatalf("expected a reported failure and no pod, got %v and %d creates", err, api.creates.Load())
	}
	for _, want := range []string{"RTX 4090 is not available right now.", `--gpu "RTX A4000"`, `--gpu "RTX A5000"`, "$0.25/h"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("missing %q in:\n%s", want, stderr)
		}
	}
	if strings.Contains(stderr, "A100") {
		t.Errorf("a GPU above the max price was suggested:\n%s", stderr)
	}
}

func TestOpenWithoutTerminalReportsCapacityAndExcludesThatGPU(t *testing.T) {
	api := &fakeAPI{catalog: testCatalog, createStatus: 400}
	config := startFakeAPI(t, api)
	key := testSSHKey(t)

	var err error
	_, stderr := captureOutput(t, func() {
		err = openWorkshop(context.Background(), config, []string{"demo", "--gpu", "RTX A4000", "--ssh-key", key})
	})

	if !errors.Is(err, errReported) || api.creates.Load() != 1 {
		t.Fatalf("expected one refused create then a reported failure, got %v and %d creates", err, api.creates.Load())
	}
	if !strings.Contains(stderr, "RunPod has no capacity for RTX A4000 right now") || !strings.Contains(stderr, `--gpu "RTX A5000"`) {
		t.Errorf("unexpected output:\n%s", stderr)
	}
	if strings.Contains(stderr, `--gpu "RTX A4000"`) {
		t.Errorf("the refused GPU was suggested again:\n%s", stderr)
	}
}

func TestCloseNeedsYesWithoutTerminal(t *testing.T) {
	created := time.Now().Add(-90 * time.Minute)
	api := &fakeAPI{pods: []map[string]any{{
		"id": "pod-1", "name": "ws-demo", "status": "RUNNING", "cost": 0.50,
		"env": map[string]string{"WS_CREATED": fmt.Sprint(created.Unix()), "WS_DEADLINE": fmt.Sprint(created.Add(3 * time.Hour).Unix()), "WS_BUDGET": "$3.00"},
	}}}
	config := startFakeAPI(t, api)
	client, _ := config.newClient()

	err := closeWorkshop(context.Background(), client, "demo", false)
	if err == nil || !strings.Contains(err.Error(), "--yes") || api.deletes.Load() != 0 {
		t.Fatalf("expected a refusal mentioning --yes and no delete, got %v and %d deletes", err, api.deletes.Load())
	}

	stdout, _ := captureOutput(t, func() {
		err = closeWorkshop(context.Background(), client, "demo", true)
	})
	if err != nil || api.deletes.Load() != 1 {
		t.Fatalf("expected one delete, got %v and %d deletes", err, api.deletes.Load())
	}
	for _, want := range []string{`Workshop demo deleted.`, "1h30", "~$0.75"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in:\n%s", want, stdout)
		}
	}
}

func TestListShowsShortGPUNames(t *testing.T) {
	created := time.Now().Add(-42 * time.Minute)
	api := &fakeAPI{catalog: testCatalog, pods: []map[string]any{{
		"id": "pod-1", "name": "ws-demo", "status": "RUNNING", "cost": 0.69,
		"gpu": map[string]any{"id": "NVIDIA GeForce RTX 4090", "count": 1},
		"env": map[string]string{"WS_CREATED": fmt.Sprint(created.Unix()), "WS_DEADLINE": fmt.Sprint(created.Add(2 * time.Hour).Unix()), "WS_BUDGET": "2h00"},
	}}}
	config := startFakeAPI(t, api)
	client, _ := config.newClient()

	var err error
	stdout, _ := captureOutput(t, func() { err = listWorkshops(context.Background(), client) })
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("list output:\n%s", stdout)
	if !strings.Contains(stdout, "RTX 4090") || strings.Contains(stdout, "NVIDIA GeForce") {
		t.Errorf("expected the short GPU name:\n%s", stdout)
	}
	for _, want := range []string{"demo", "RUNNING", "0h42", "~$0.48", "2h00 · deleted at"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in:\n%s", want, stdout)
		}
	}
}

func TestOpenWithoutTerminalNeverGoesAboveMaxPrice(t *testing.T) {
	api := &fakeAPI{catalog: testCatalog}
	config := startFakeAPI(t, api)
	key := testSSHKey(t)

	var err error
	_, stderr := captureOutput(t, func() {
		err = openWorkshop(context.Background(), config, []string{"demo", "--gpu", "RTX 4090", "--max-price", "0.10", "--ssh-key", key})
	})

	if !errors.Is(err, errReported) || api.creates.Load() != 0 {
		t.Fatalf("expected a reported failure and no pod, got %v and %d creates", err, api.creates.Load())
	}
	if !strings.Contains(stderr, "Nothing available within $0.10/h") || !strings.Contains(stderr, "raise --max-price") {
		t.Errorf("unexpected output:\n%s", stderr)
	}
}

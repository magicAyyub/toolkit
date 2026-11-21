package workshop

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"toolkit/internal/runpod"
)

// fakeRunPod serves just enough of the API to create a pod and watch it.
type fakeRunPod struct {
	createStatus int      // HTTP status for POST /v2/pods; 0 means 201
	podStatuses  []string // status per GET /v2/pods/pod-1, the last one repeats
	directSSH    bool     // whether RUNNING pods report a direct SSH endpoint...
	directFrom   int      // ...from this GET on (0-based)
	systemLogs   []string
	gets         atomic.Int32
	deletes      atomic.Int32
}

func (f *fakeRunPod) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v2/pods":
		if f.createStatus != 0 {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(f.createStatus)
			fmt.Fprintf(w, `{"title":"Bad Request","status":%d,"detail":"no capacity for this GPU"}`, f.createStatus)
			return
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"id": "pod-1", "name": "ws-test", "status": "PROVISIONING", "cost": 0.20})
	case r.Method == http.MethodGet && r.URL.Path == "/v2/pods/pod-1":
		call := int(f.gets.Add(1)) - 1
		pod := map[string]any{"id": "pod-1", "name": "ws-test", "status": f.podStatuses[min(call, len(f.podStatuses)-1)], "cost": 0.20}
		if pod["status"] == "RUNNING" && f.directSSH && call >= f.directFrom {
			pod["ssh"] = map[string]any{"direct": map[string]any{"host": "203.0.113.7", "port": 2222, "username": "root"}}
		}
		json.NewEncoder(w).Encode(pod)
	case r.Method == http.MethodGet && r.URL.Path == "/v2/pods/pod-1/logs":
		if r.URL.Query().Get("source") != "system" {
			http.Error(w, "unexpected source", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for i, line := range f.systemLogs {
			payload, _ := json.Marshal(map[string]string{"ts": "2026-11-20T15:00:00Z", "source": "system", "line": line})
			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", i, payload)
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done() // keep the stream open like the real one
	case r.Method == http.MethodDelete && r.URL.Path == "/v2/pods/pod-1":
		f.deletes.Add(1)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unexpected request "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}
}

func newFakeClient(t *testing.T, fake *fakeRunPod) *runpod.Client {
	t.Helper()
	api := httptest.NewServer(fake)
	t.Cleanup(api.Close)
	client := runpod.NewClient("test-key")
	client.BaseURL = api.URL
	return client
}

var testGPU = runpod.GPUType{ID: "NVIDIA RTX A4000", Name: "RTX A4000", Secure: true, Price: runpod.GPUPrice{Secure: 0.25}, Availability: "HIGH"}

func testSpec() Spec {
	return Spec{Name: "test", GPU: testGPU, Cloud: "SECURE", Budget: Budget{time: time.Hour}, IdleTimeout: 30 * time.Minute, PublicKey: "ssh-ed25519 AAAA test"}
}

func TestCreateReportsCapacityErrors(t *testing.T) {
	client := newFakeClient(t, &fakeRunPod{createStatus: 400})

	_, err := Create(t.Context(), client, testSpec())
	var capacity *CapacityError
	if !errors.As(err, &capacity) || capacity.GPU.ID != testGPU.ID || capacity.Detail != "no capacity for this GPU" {
		t.Errorf("expected a CapacityError for %s, got %v", testGPU.ID, err)
	}
}

func TestWaitUntilReadyDeletesPodThatCannotStart(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cases := map[string]*fakeRunPod{
		"image pull fails, status stays PROVISIONING": {
			podStatuses: []string{"PROVISIONING"},
			systemLogs: []string{
				"create container runpod/pytorch:1.0.2-cu1281-torch280-ubuntu2404",
				"error creating container: Get https://registry-1.docker.io/v2/: dial tcp: connect: no route to host",
			},
		},
		"container in ERROR state": {podStatuses: []string{"STARTING", "ERROR"}},
	}
	defaultInterval := pollInterval
	pollInterval = 10 * time.Millisecond
	t.Cleanup(func() { pollInterval = defaultInterval })

	for name, fake := range cases {
		t.Run(name, func(t *testing.T) {
			client := newFakeClient(t, fake)
			info, err := Create(t.Context(), client, testSpec())
			if err != nil {
				t.Fatal(err)
			}

			var phases []string
			started := time.Now()
			_, _, err = WaitUntilReady(t.Context(), client, info, "/nonexistent/key.pub", func(event Event) {
				if event.Phase != "" {
					phases = append(phases, event.Phase)
				}
			})

			var abandoned *AbandonedError
			if !errors.As(err, &abandoned) || abandoned.DeleteErr != nil || abandoned.PodID != "pod-1" {
				t.Fatalf("expected an AbandonedError with the pod deleted, got %v", err)
			}
			if fake.deletes.Load() != 1 {
				t.Errorf("expected the pod to be deleted once, got %d deletes", fake.deletes.Load())
			}
			if len(phases) == 0 || phases[0] != "Starting the machine" {
				t.Errorf("unexpected phases %q", phases)
			}
			if elapsed := time.Since(started); elapsed > 5*time.Second {
				t.Errorf("should give up quickly, took %s", elapsed)
			}
		})
	}
}

func TestEndpointWaitsForSSHPort(t *testing.T) {
	defaultInterval := pollInterval
	pollInterval = 10 * time.Millisecond
	t.Cleanup(func() { pollInterval = defaultInterval })

	t.Run("port appears after a moment", func(t *testing.T) {
		// What RunPod does right after a session drops: RUNNING, but no public
		// SSH port for a moment.
		fake := &fakeRunPod{podStatuses: []string{"RUNNING"}, directSSH: true, directFrom: 2}
		client := newFakeClient(t, fake)

		_, endpoint, err := Endpoint(t.Context(), client, Info{Name: "test", Pod: runpod.Pod{ID: "pod-1"}})
		if err != nil || endpoint.Port != 2222 {
			t.Fatalf("got %+v, %v", endpoint, err)
		}
		if fake.gets.Load() != 3 {
			t.Errorf("expected 3 polls, got %d", fake.gets.Load())
		}
	})

	t.Run("stopped pod fails immediately", func(t *testing.T) {
		fake := &fakeRunPod{podStatuses: []string{"EXITED"}}
		client := newFakeClient(t, fake)

		_, _, err := Endpoint(t.Context(), client, Info{Name: "test", Pod: runpod.Pod{ID: "pod-1"}})
		if err == nil || !strings.Contains(err.Error(), "EXITED") || fake.gets.Load() != 1 {
			t.Errorf("expected an immediate EXITED error, got %v after %d polls", err, fake.gets.Load())
		}
	})
}

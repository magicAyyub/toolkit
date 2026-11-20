package workshop

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"toolkit/internal/runpod"
)

// fakeRunPod serves just enough of the API for Open to create a pod that
// never becomes reachable, in the way the scenario describes.
type fakeRunPod struct {
	podStatus  string
	systemLogs []string
	deletes    atomic.Int32
}

func (f *fakeRunPod) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	pod := map[string]any{"id": "pod-1", "name": "ws-test", "status": f.podStatus, "cost": 0.20, "env": map[string]string{}}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v2/pods":
		json.NewEncoder(w).Encode(map[string]any{"pods": []any{}, "pagination": map[string]any{"hasNextPage": false}})
	case r.Method == http.MethodGet && r.URL.Path == "/v2/catalog/gpus":
		json.NewEncoder(w).Encode(map[string]any{"gpus": []runpod.GPUType{{
			ID: "NVIDIA RTX A4000", Name: "RTX A4000", Secure: true, Community: true,
			Price: runpod.GPUPrice{Secure: 0.25, Community: 0.20}, Availability: "HIGH",
		}}})
	case r.Method == http.MethodPost && r.URL.Path == "/v2/pods":
		pod["status"] = "PROVISIONING"
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(pod)
	case r.Method == http.MethodGet && r.URL.Path == "/v2/pods/pod-1":
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

func TestOpenDeletesPodThatCannotStart(t *testing.T) {
	cases := map[string]*fakeRunPod{
		"image pull fails, status stays PROVISIONING": {
			podStatus: "PROVISIONING",
			systemLogs: []string{
				"create container runpod/pytorch:1.0.2-cu1281-torch280-ubuntu2404",
				"error creating container: Get https://registry-1.docker.io/v2/: dial tcp: connect: no route to host",
			},
		},
		"container in ERROR state": {podStatus: "ERROR"},
	}

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	keyPath := filepath.Join(t.TempDir(), "key")
	if output, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", keyPath).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, output)
	}

	for name, fake := range cases {
		t.Run(name, func(t *testing.T) {
			api := httptest.NewServer(fake)
			defer api.Close()
			client := runpod.NewClient("test-key")
			client.BaseURL = api.URL

			started := time.Now()
			err := Open(t.Context(), client, OpenOptions{
				Name: "test", GPU: "RTX A4000", MaxPrice: 1, Budget: Budget{time: time.Hour},
				IdleTimeout: 30 * time.Minute, SSHKey: keyPath + ".pub",
			})

			if err == nil || !strings.Contains(err.Error(), "workshop not opened") {
				t.Errorf("expected Open to give up, got %v", err)
			}
			if fake.deletes.Load() != 1 {
				t.Errorf("expected the pod to be deleted once, got %d deletes", fake.deletes.Load())
			}
			if elapsed := time.Since(started); elapsed > 15*time.Second {
				t.Errorf("Open should give up quickly, took %s", elapsed)
			}
		})
	}
}

// Package runpod is a small client for the parts of the RunPod REST API v2
// that the toolkit uses: pods and the GPU catalog.
package runpod

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const DefaultBaseURL = "https://api.runpod.io"

type Client struct {
	apiKey  string
	BaseURL string // overridable for tests
	http    *http.Client
}

func NewClient(apiKey string) *Client {
	return &Client{
		apiKey:  apiKey,
		BaseURL: DefaultBaseURL,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

type Pod struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Status    string            `json:"status"` // PROVISIONING, STARTING, RUNNING, EXITED, ERROR, TERMINATED
	Image     string            `json:"image"`
	Env       map[string]string `json:"env"`
	Cloud     string            `json:"cloud"`
	Cost      float64           `json:"cost"` // USD per hour
	CreatedAt time.Time         `json:"createdAt"`
	GPU       *PodGPU           `json:"gpu"`
	SSH       PodSSH            `json:"ssh"`
}

type PodGPU struct {
	ID    string `json:"id"`
	Count int    `json:"count"`
}

type PodSSH struct {
	Proxy  *SSHEndpoint `json:"proxy"`
	Direct *SSHEndpoint `json:"direct"` // set once the pod has a public port mapped to 22/tcp
}

type SSHEndpoint struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
}

type CreatePodRequest struct {
	Name  string            `json:"name"`
	Image string            `json:"image"`
	GPU   GPURequest        `json:"gpu"`
	Cloud string            `json:"cloud"`
	Disk  int               `json:"disk"`
	Ports []string          `json:"ports"`
	Env   map[string]string `json:"env"`
	Cmd   []string          `json:"cmd,omitempty"`
}

type GPURequest struct {
	ID    string `json:"id"`
	Count int    `json:"count"`
}

type GPUType struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Memory       int      `json:"memory"` // VRAM in GB
	Secure       bool     `json:"secure"`
	Community    bool     `json:"community"`
	Price        GPUPrice `json:"price"`        // USD per hour for one GPU
	Availability string   `json:"availability"` // NONE, LOW, MEDIUM, HIGH
}

type GPUPrice struct {
	Secure    float64 `json:"secure"`
	Community float64 `json:"community"`
}

// APIError is the RFC 9457 problem document RunPod returns on failure.
type APIError struct {
	Status int      `json:"status"`
	Title  string   `json:"title"`
	Detail string   `json:"detail"`
	Errors []string `json:"errors"`
}

func decodeAPIError(status int, body []byte) *APIError {
	apiErr := &APIError{Status: status, Title: http.StatusText(status)}
	// Best effort: the body is usually a problem document, but proxies can return anything.
	_ = json.Unmarshal(body, apiErr)
	apiErr.Status = status
	return apiErr
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("runpod: %d %s", e.Status, e.Title)
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	if len(e.Errors) > 0 {
		msg += " (" + strings.Join(e.Errors, "; ") + ")"
	}
	return msg
}

func (c *Client) ListPods(ctx context.Context) ([]Pod, error) {
	var pods []Pod
	cursor := ""
	for {
		query := url.Values{}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		var page struct {
			Pods       []Pod `json:"pods"`
			Pagination struct {
				NextCursor  string `json:"nextCursor"`
				HasNextPage bool   `json:"hasNextPage"`
			} `json:"pagination"`
		}
		if err := c.do(ctx, http.MethodGet, "/v2/pods?"+query.Encode(), nil, &page); err != nil {
			return nil, err
		}
		pods = append(pods, page.Pods...)
		if !page.Pagination.HasNextPage {
			return pods, nil
		}
		cursor = page.Pagination.NextCursor
	}
}

func (c *Client) GetPod(ctx context.Context, id string) (Pod, error) {
	var pod Pod
	err := c.do(ctx, http.MethodGet, "/v2/pods/"+url.PathEscape(id), nil, &pod)
	return pod, err
}

func (c *Client) CreatePod(ctx context.Context, req CreatePodRequest) (Pod, error) {
	var pod Pod
	err := c.do(ctx, http.MethodPost, "/v2/pods", req, &pod)
	return pod, err
}

// DeletePod terminates the pod. This is irreversible: the container disk is destroyed.
func (c *Client) DeletePod(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v2/pods/"+url.PathEscape(id), nil, nil)
}

// StreamPodLogs calls onLine for each log line of the given source ("system"
// for host lifecycle events such as image pulls, "container" for the
// container's output), starting with up to 100 past lines. It blocks until ctx
// is cancelled or the server ends the stream.
func (c *Client) StreamPodLogs(ctx context.Context, id, source string, onLine func(line string)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.BaseURL+"/v2/pods/"+url.PathEscape(id)+"/logs?source="+url.QueryEscape(source), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "text/event-stream")

	// No client timeout: the stream is meant to stay open, ctx bounds it.
	resp, err := http.DefaultClient.Do(req)
	if ctx.Err() != nil {
		return nil // cancelled by the caller: not a failure
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return decodeAPIError(resp.StatusCode, body)
	}

	// Server-Sent Events: each event carries a JSON payload on a "data:" line.
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		payload, isData := strings.CutPrefix(scanner.Text(), "data:")
		if !isData {
			continue
		}
		var event struct {
			Line string `json:"line"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(payload)), &event) == nil {
			onLine(event.Line)
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	return scanner.Err()
}

// ListGPUTypes returns the GPU catalog with current pod availability for the given cloud
// ("SECURE" or "COMMUNITY").
func (c *Client) ListGPUTypes(ctx context.Context, cloud string) ([]GPUType, error) {
	query := url.Values{}
	query.Set("include", "AVAILABILITY")
	query.Set("product", "POD")
	query.Set("cloud", cloud)
	var catalog struct {
		GPUs []GPUType `json:"gpus"`
	}
	err := c.do(ctx, http.MethodGet, "/v2/catalog/gpus?"+query.Encode(), nil, &catalog)
	return catalog.GPUs, err
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reqBody = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reqBody)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode >= 300 {
		return decodeAPIError(resp.StatusCode, respBody)
	}

	if out == nil || len(respBody) == 0 {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("runpod: decoding response from %s %s: %w", method, path, err)
	}
	return nil
}

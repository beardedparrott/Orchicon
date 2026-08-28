package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// Client is the control-plane side of the daemon API. It talks to the
// runtime daemon over a unix socket (the daemon's socket is bind-mounted
// into the supervisor container). The control plane can only ever issue
// the narrow, validated operations the daemon exposes.
type Client struct {
	socketPath string
	instance   string // owning instance ("dev"/"prod") — scopes List/Create
	hc         *http.Client
}

// NewClient returns a client for the daemon socket at socketPath (e.g.
// /var/run/orchicon-runtime/runtime.sock inside the supervisor container).
// instance labels every runtime container this client creates and scopes
// List to that instance, so two control planes sharing one daemon never
// reap each other's containers.
func NewClient(socketPath, instance string) *Client {
	dial := func(ctx context.Context, _ string, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socketPath)
	}
	return &Client{
		socketPath: socketPath,
		instance:   instance,
	hc: &http.Client{
		Transport: &http.Transport{
			DialContext:         dial,
			MaxIdleConns:        4,
			IdleConnTimeout:     30 * time.Second,
			ResponseHeaderTimeout: 120 * time.Second,
		},
		Timeout: 0, // exec streams are long-lived
	},
	}
}

// Create ensures a runtime container exists for the workflow.
func (c *Client) Create(ctx context.Context, req CreateRequest) (*CreateResponse, error) {
	if req.InstanceID == "" {
		req.InstanceID = c.instance
	}
	var out CreateResponse
	if err := c.postJSON(ctx, "/v1/runtimes", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// List returns the active runtime container names for THIS instance (for
// boot-time adopt).
func (c *Client) List(ctx context.Context) ([]string, error) {
	var out ListResponse
	if err := c.getJSON(ctx, "/v1/runtimes?instance="+c.instance, &out); err != nil {
		return nil, err
	}
	return out.Runtimes, nil
}

// Kill removes a workflow's runtime container (idempotent).
func (c *Client) Kill(ctx context.Context, workflowID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, "http://runtime"+"/v1/runtimes/"+workflowID, nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return readError(resp.Body)
	}
	return nil
}

// Ready returns true if the daemon socket answers /v1/health.
type StreamDroppedError struct{ Err error }
func (e *StreamDroppedError) Error() string { return "build log stream disconnected: " + e.Err.Error() }
func IsStreamDropped(err error) bool { _, ok := err.(*StreamDroppedError); return ok }

func (c *Client) Ready(ctx context.Context) bool {
	var out map[string]string
	return c.getJSON(ctx, "/v1/health", &out) == nil
}

// Images returns the daemon's base image + allowlisted stock images.
func (c *Client) Images(ctx context.Context) (*ImageList, error) {
	var out ImageList
	if err := c.getJSON(ctx, "/v1/images", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// BuildImage streams a `docker build` for a custom runtime image. Log
// chunks arrive via fn; the final event carries the exit code. Returns
// the build's exit code (0 = success).
func (c *Client) BuildImage(ctx context.Context, req BuildRequest, fn func(AgentEvent) error) (int, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return 1, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://runtime"+"/v1/images", bytes.NewReader(body))
	if err != nil {
		return 1, err
	}
	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return 1, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return 1, fmt.Errorf("runtime image build: %s", e["error"])
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	exit := 1
	sawExit := false
	for sc.Scan() {
		var ev AgentEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		if fn != nil {
			if err := fn(ev); err != nil {
				return 1, err
			}
		}
		if ev.Event == "exit" {
			sawExit = true
			exit = ev.ExitCode
			if ev.Error != "" {
				return exit, fmt.Errorf("runtime image build: %s", ev.Error)
			}
			break
		}
	}
	if err := sc.Err(); err != nil {
		return 1, &StreamDroppedError{Err: err}
	}
	if !sawExit {
		return 1, &StreamDroppedError{Err: fmt.Errorf("stream ended without exit event")}
	}
	return exit, nil
}

// RemoveImage removes a locally-built runtime image (docker rmi, best-effort).
func (c *Client) CancelBuild(ctx context.Context, tag string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, "http://runtime"+"/v1/images/build?tag="+url.QueryEscape(tag), nil)
	if err != nil { return err }
	resp, err := c.hc.Do(req)
	if err != nil { return err }
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK { return readError(resp.Body) }
	return nil
}
func (c *Client) IsBuilding(ctx context.Context, tag string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://runtime"+"/v1/images/build?tag="+url.QueryEscape(tag), nil)
	if err != nil { return false, err }
	resp, err := c.hc.Do(req)
	if err != nil { return false, err }
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK { return false, readError(resp.Body) }
	var out map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil { return false, err }
	return out["status"] == "building", nil
}
func (c *Client) RemoveImage(ctx context.Context, ref string) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		"http://runtime"+"/v1/images?ref="+url.QueryEscape(ref), nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return readError(resp.Body)
	}
	return nil
}

func (c *Client) postJSON(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://runtime"+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return readError(resp.Body)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://runtime"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return readError(resp.Body)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func readError(r io.Reader) error {
	var e map[string]string
	_ = json.NewDecoder(r).Decode(&e)
	if msg, ok := e["error"]; ok {
		return fmt.Errorf("runtime daemon: %s", msg)
	}
	return fmt.Errorf("runtime daemon: request failed")
}

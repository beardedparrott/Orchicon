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
				ResponseHeaderTimeout: 30 * time.Second,
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

// Exec streams a dispatch into the workflow's runtime container. Events
// (stdout/stderr chunks, exit) are delivered to fn in order. fn may
// return an error to abort the stream (the underlying exec is not killed
// unless the context is cancelled). Returns the child's exit code.
func (c *Client) Exec(ctx context.Context, workflowID string, req ExecRequest, fn func(AgentEvent) error) (int, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return 0, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://runtime"+"/v1/runtimes/"+workflowID+"/exec", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return 1, fmt.Errorf("runtime exec: %s", e["error"])
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024)
	exit := 1
	for sc.Scan() {
		var ev AgentEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		if fn != nil {
			if err := fn(ev); err != nil {
				return 0, err
			}
		}
		if ev.Event == "exit" {
			exit = ev.ExitCode
			break
		}
		if ev.Event == "error" {
			return 1, fmt.Errorf("runtime exec: %s", ev.Error)
		}
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return exit, nil
}

// Signal sends a signal to a running exec inside the workflow runtime.
func (c *Client) Signal(ctx context.Context, workflowID string, req SignalRequest) error {
	var out map[string]string
	return c.postJSON(ctx, "/v1/runtimes/"+workflowID+"/signal", req, &out)
}

// ExecAlive reports whether an exec is still running inside a workflow's
// runtime container. Returns (false, nil) when the container is gone or
// the exec is not alive; an error only when the daemon itself is
// unreachable (the caller should skip, not reap).
func (c *Client) ExecAlive(ctx context.Context, workflowID, execID string) (bool, error) {
	var out struct {
		Alive     bool `json:"alive"`
		Container bool `json:"container"`
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://runtime"+"/v1/runtimes/"+workflowID+"/execs/"+execID, nil)
	if err != nil {
		return false, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, readError(resp.Body)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, err
	}
	return out.Alive, nil
}

// Ready returns true if the daemon socket answers /v1/health.
func (c *Client) Ready(ctx context.Context) bool {
	var out map[string]string
	return c.getJSON(ctx, "/v1/health", &out) == nil
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

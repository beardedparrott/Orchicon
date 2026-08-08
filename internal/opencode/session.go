// Session transport for the opencode adapter (Stage 3).
//
// The adapter drives worker executions through a persistent opencode
// server/session instead of a one-shot `opencode run` subprocess. A serve
// instance (one always-on host serve for the in-process population, one
// per workflow-run container for containerized executions) owns each
// execution's session and its agent loop. This package is the HTTP+SSE
// client for that serve:
//
//   - POST /session                 -> create a session (per execution)
//   - POST /session/:id/prompt_async -> send a message (goal, nudge, human)
//   - POST /session/:id/abort       -> cancel the running turn
//   - POST /session/:id/permissions/:id -> auto-approve a permission ask
//   - GET  /event                   -> the server's SSE event bus
//
// The SSE bus is the SAME event source `opencode run --format json`
// renders: run.ts subscribes via the SDK and re-emits each bus event as
// `{type, timestamp, sessionID, part}`. legacyEventFromBus performs that
// mapping in Go so the adapter's existing parseEvent pipeline (text /
// tool_use / step_start / step_finish / reasoning / error) consumes the
// session stream unchanged.
//
// Sessions are scoped to a project directory via the x-opencode-directory
// header (moved to a `directory` query param on GETs, mirroring the SDK's
// request interceptor). The serve's bash/file tools resolve against that
// directory, which is what makes one shared host serve able to host
// sessions across many project dirs.
package opencode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// defaultServeUsername is the HTTP basic-auth username opencode serves
// with (OPENCODE_SERVER_USERNAME defaults to "opencode").
const defaultServeUsername = "opencode"

// ServePasswordEnv is the env var the host serve / runtime supervisor
// sets to protect its HTTP API with basic auth.
const ServePasswordEnv = "OPENCODE_SERVER_PASSWORD"

// BusEvent is one event from the opencode server's /event SSE stream.
// The shape matches the SDK's event objects ({id, type, properties}).
type BusEvent struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Properties map[string]any `json:"properties"`
}

// SessionClient is an HTTP+SSE client for one opencode serve instance.
// All requests are scoped to the `directory` project path.
type SessionClient struct {
	baseURL   string
	username  string
	password  string
	directory string
	hc        *http.Client
}

// NewSessionClient builds a client for serve at baseURL (e.g.
// http://127.0.0.1:PORT). password may be empty (unauthenticated serve).
// directory is the project path every session created through this client
// is scoped to (may be empty for an unscoped client).
func NewSessionClient(baseURL, password, directory string) *SessionClient {
	return &SessionClient{
		baseURL:   strings.TrimSuffix(baseURL, "/"),
		username:  defaultServeUsername,
		password:  password,
		directory: directory,
		// No global timeout: the /event subscription is long-lived. API
		// calls carry their own per-request context deadlines.
		hc: &http.Client{},
	}
}

// BaseURL returns the serve base URL this client talks to.
func (c *SessionClient) BaseURL() string { return c.baseURL }

// Healthy reports whether the serve answers /global/health.
func (c *SessionClient) Healthy(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := c.newRequest(ctx, http.MethodGet, "/global/health", nil, nil)
	if err != nil {
		return false
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// CreateSession creates a session on the serve, scoped to the client's
// directory. The permission ruleset mirrors `opencode run`'s non-interactive
// mode: the agent may never ask for permission or enter/exit plan mode, so
// an unattended execution can't block on a prompt. A generous timeout
// absorbs the serve's cold-start window (providers/MCP load) on the first
// dispatch after a runtime container starts.
func (c *SessionClient) CreateSession(ctx context.Context, title string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	body := map[string]any{
		"title": title,
		"permission": []map[string]any{
			{"permission": "question", "action": "deny", "pattern": "*"},
			{"permission": "plan_enter", "action": "deny", "pattern": "*"},
			{"permission": "plan_exit", "action": "deny", "pattern": "*"},
		},
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/session", body, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", fmt.Errorf("opencode session create: empty session id")
	}
	return out.ID, nil
}

// SendMessage appends a user message to the session via prompt_async
// (fire-and-forget: 204, the reply arrives on the SSE bus). The server
// serializes prompts per session, so a message sent while a turn is in
// flight is queued and answered at the next turn boundary — the nudge /
// mid-run chat primitive. system is the per-message system prompt (the
// worker's composed prompt — opencode applies it to THIS turn only, so it
// must be sent with every message). modelRef is "provider/model" (empty =
// the session's current model).
func (c *SessionClient) SendMessage(ctx context.Context, sessionID, system, modelRef, text string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	body := map[string]any{
		"parts": []map[string]any{{"type": "text", "text": text}},
	}
	if system != "" {
		body["system"] = system
	}
	if modelRef != "" {
		if provider, model, ok := splitModelRef(modelRef); ok {
			body["model"] = map[string]any{"providerID": provider, "modelID": model}
		}
	}
	// 204 No Content on success.
	if err := c.do(ctx, http.MethodPost, "/session/"+sessionID+"/prompt_async", body); err != nil {
		return fmt.Errorf("opencode session send: %w", err)
	}
	return nil
}

// Abort cancels the session's running turn. The session (and its history)
// is kept; a subsequent SendMessage starts a new turn.
func (c *SessionClient) Abort(ctx context.Context, sessionID string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := c.do(ctx, http.MethodPost, "/session/"+sessionID+"/abort", map[string]any{}); err != nil {
		return fmt.Errorf("opencode session abort: %w", err)
	}
	return nil
}

// ReplyPermission answers a permission.asked event with "once" — the
// server-side equivalent of `opencode run --auto`, so tool calls never
// block an unattended execution.
func (c *SessionClient) ReplyPermission(ctx context.Context, sessionID, permissionID string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := c.do(ctx, http.MethodPost, "/session/"+sessionID+"/permissions/"+permissionID,
		map[string]any{"response": "once"}); err != nil {
		return fmt.Errorf("opencode session permission reply: %w", err)
	}
	return nil
}

// Subscribe opens the serve's /event SSE stream and returns a Subscription
// that yields bus events (for ALL sessions — callers filter by session id).
// The stream blocks until ctx is cancelled or the connection drops.
func (c *SessionClient) Subscribe(ctx context.Context) (*Subscription, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/event", nil, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("opencode session subscribe: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("opencode session subscribe: http %d", resp.StatusCode)
	}
	sub := &Subscription{events: make(chan BusEvent, 256), done: make(chan struct{}), once: make(chan struct{}), body: resp.Body}
	go sub.read(ctx, resp.Body)
	return sub, nil
}

// Subscription is a parsed SSE event stream from an opencode serve.
type Subscription struct {
	events chan BusEvent
	done   chan struct{}
	once   chan struct{}
	body   io.Closer
}

// Events returns the decoded bus events as they arrive.
func (s *Subscription) Events() <-chan BusEvent { return s.events }

// Done closes when the underlying stream ends or Close is called.
func (s *Subscription) Done() <-chan struct{} { return s.done }

// Close terminates the subscription. Closing the underlying HTTP body
// unblocks a reader that is parked on the socket.
func (s *Subscription) Close() {
	select {
	case <-s.once:
	default:
		close(s.once)
	}
	if s.body != nil {
		_ = s.body.Close()
	}
}

func (s *Subscription) read(ctx context.Context, body io.ReadCloser) {
	defer body.Close()
	defer close(s.done)
	defer close(s.events)
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var data bytes.Buffer
	flush := func() {
		if data.Len() == 0 {
			return
		}
		var evt BusEvent
		if err := json.Unmarshal(data.Bytes(), &evt); err == nil && evt.Type != "" {
			select {
			case s.events <- evt:
			case <-s.once:
			}
		}
		data.Reset()
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, ":"):
			// SSE comment / keepalive.
			continue
		case line == "":
			flush()
		case strings.HasPrefix(line, "data:"):
			payload := strings.TrimPrefix(line, "data:")
			payload = strings.TrimPrefix(payload, " ")
			data.WriteString(payload)
		default:
			// Ignore `event:` / `id:` metadata lines; the data frame
			// carries the JSON.
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-s.once:
			return
		default:
		}
	}
}

// legacyEventFromBus maps a bus event to the legacy {type, part} event
// object the adapter's parseEvent pipeline consumes — the exact mapping
// `opencode run --format json` applies in run.ts (text only at part end,
// tool only when completed/errored, step-start/step-finish, reasoning at
// part end, and session errors). ok=false means "not a telemetry event".
func legacyEventFromBus(evt BusEvent) (map[string]any, bool) {
	props := evt.Properties
	switch evt.Type {
	case "message.part.updated":
		part, _ := props["part"].(map[string]any)
		if part == nil {
			return nil, false
		}
		ptype, _ := part["type"].(string)
		switch ptype {
		case "tool":
			state, _ := part["state"].(map[string]any)
			status, _ := state["status"].(string)
			if status != "completed" && status != "error" {
				return nil, false
			}
			return map[string]any{"type": "tool_use", "part": part}, true
		case "step-start":
			return map[string]any{"type": "step_start", "part": part}, true
		case "step-finish":
			return map[string]any{"type": "step_finish", "part": part}, true
		case "text", "reasoning":
			// Only the completed part (time.end set) is emitted, matching
			// run.ts; mid-generation deltas arrive as part.delta events.
			tm, _ := part["time"].(map[string]any)
			if _, hasEnd := tm["end"]; !hasEnd {
				return nil, false
			}
			return map[string]any{"type": ptype, "part": part}, true
		}
	case "session.error":
		return map[string]any{"type": "error", "error": props["error"]}, true
	}
	return nil, false
}

// newRequest builds an authenticated request, scoping it to the client's
// directory: the x-opencode-directory header on writes, the `directory`
// query param on GETs (mirroring the SDK's request interceptor).
func (c *SessionClient) newRequest(ctx context.Context, method, path string, body any, query url.Values) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	u := c.baseURL + path
	if query != nil {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(c.username, c.password)
	if c.directory != "" {
		enc := url.QueryEscape(c.directory)
		if method == http.MethodGet || method == http.MethodHead {
			q := req.URL.Query()
			q.Set("directory", enc)
			req.URL.RawQuery = q.Encode()
		} else {
			req.Header.Set("x-opencode-directory", enc)
		}
	}
	return req, nil
}

// do performs a request expecting 2xx (no body read).
func (c *SessionClient) do(ctx context.Context, method, path string, body any) error {
	req, err := c.newRequest(ctx, method, path, body, nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("opencode serve %s %s: http %d", method, path, resp.StatusCode)
	}
	return nil
}

// doJSON performs a request and decodes the JSON response.
func (c *SessionClient) doJSON(ctx context.Context, method, path string, body, out any) error {
	req, err := c.newRequest(ctx, method, path, body, nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("opencode serve %s %s: http %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
	}
	return nil
}

// splitModelRef splits "provider/model" into (provider, model). Returns
// ok=false when there is no "/" separator.
func splitModelRef(ref string) (provider, model string, ok bool) {
	ref = strings.TrimSpace(ref)
	if i := strings.IndexByte(ref, '/'); i > 0 {
		return ref[:i], ref[i+1:], true
	}
	return "", "", false
}

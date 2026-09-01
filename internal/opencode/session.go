// Session transport for the opencode adapter (Stage 3).
//
// The adapter drives worker executions through persistent opencode
// server/session — the ONE execution transport (the legacy `opencode
// run` subprocess path was removed). A serve
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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/adapter"
)

// defaultServeUsername is the HTTP basic-auth username opencode serves
// with (OPENCODE_SERVER_USERNAME defaults to "opencode").
const defaultServeUsername = "opencode"

// ServePasswordEnv is the env var the host serve / runtime supervisor
// sets to protect its HTTP API with basic auth.
const ServePasswordEnv = "OPENCODE_SERVER_PASSWORD"

// defaultMCPProbeTimeout bounds a single MCP-usability probe so a slow (but
// alive) serve is not treated as wedged by the watchdog and restarted in a
// churn. Env override ORCHICON_ASK_MCP_PROBE_TIMEOUT is a dev/test knob.
const defaultMCPProbeTimeout = 8 * time.Second

func askMCPProbeTimeout() time.Duration {
	if v := os.Getenv("ORCHICON_ASK_MCP_PROBE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultMCPProbeTimeout
}

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
	// hc is the client for short-lived API calls. It uses a DEDICATED
	// http.Transport (ADR-0002 D5) so this serve's connections never contend
	// on the package-level http.DefaultTransport shared by every SessionClient.
	hc *http.Client
	// sseHC is the client for long-lived /event SSE subscriptions. It uses a
	// separate keep-alive-disabled transport so a parked stream never occupies
	// the API idle pool (D5).
	sseHC *http.Client
}

// ErrSessionNotFound marks a 404 from the serve for a session-scoped
// request: the session id is no longer valid on that serve (its data dir
// was wiped or the serve restarted against a fresh store). Callers
// re-create the session and re-seed context rather than failing the turn.
var ErrSessionNotFound = errors.New("opencode serve: session not found")

// NewSessionClient builds a client for serve at baseURL (e.g.
// http://127.0.0.1:PORT). password may be empty (unauthenticated serve).
// directory is the project path every session created through this client
// is scoped to (may be empty for an unscoped client).
func NewSessionClient(baseURL, password, directory string) *SessionClient {
	baseURL = strings.TrimSuffix(baseURL, "/")
	// Dedicated transport (ADR-0002 D5). Every SessionClient used to be built
	// with &http.Client{}, which SHARES the package-level http.DefaultTransport
	// (MaxIdleConnsPerHost=2, MaxIdleConns=100). With many concurrent sessions
	// each doing an API call plus a long-lived /event stream against the ONE
	// host serve, that pooled to 2 idle conns a host and sessions contended.
	// A dedicated transport sized to the concurrent-session ceiling isolates
	// this serve's traffic from every other client's. There is deliberately
	// NO per-request global timeout: the /event subscription is long-lived and
	// API calls carry their own context deadlines.
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   256,
		MaxConnsPerHost:       256,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	// /event SSE pushes ONE long-lived stream per subscription; keep-alive is
	// pointless and a parked stream must never land in the shared idle pool.
	// A separate keep-alive-disabled client keeps long-lived streams off the
	// API pool (D5).
	sseTransport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		DisableKeepAlives:   true,
		MaxIdleConnsPerHost: 256,
	}
	return &SessionClient{
		baseURL:   baseURL,
		username:  defaultServeUsername,
		password:  password,
		directory: directory,
		hc:        &http.Client{Transport: transport},
		sseHC:     &http.Client{Transport: sseTransport},
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

// probeServe exercises the serve's full readiness surface: it must answer
// /global/health AND accept a real session-create round-trip. A cold-starting
// serve answers health before its session machinery is up, so health alone is
// not "usable" for dispatch. The probe session is aborted after creation so no
// probe residue accumulates.
func (c *SessionClient) probeServe(ctx context.Context) error {
	if !c.Healthy(ctx) {
		return fmt.Errorf("opencode serve not healthy (%s)", c.baseURL)
	}
	sid, err := c.CreateSession(ctx, "orchicon-serving-probe")
	if err != nil {
		return fmt.Errorf("opencode serve session create failed: %w", err)
	}
	_ = c.Abort(ctx, sid)
	return nil
}

// ProbeUsable is the L1 serve-readiness probe (the workflow run-start gate):
// the serve must answer /global/health AND accept a real session-create
// round-trip. A cold-starting serve answers health before its session
// machinery is up, so health alone is not "usable" for dispatch.
func (c *SessionClient) ProbeUsable(ctx context.Context) error {
	return c.probeServe(ctx)
}

// MCPHealthy reports whether the serve's MCP-enabled agent session machinery is
// usable (ADR-0002 D1 — the MCP watchdog). The Orchicon MCP is loaded into the
// serve at startup, and a wedged/unusable MCP is INVISIBLE to /global/health
// (which only proves the process is up). A session-create round-trip exercises
// the serve's agent session + MCP configuration; a session that cannot be
// created/aborted means the serve is not usable for dispatch. This is the
// plane-level watchdog gate: `HostServe.Watch` restarts a serve that passes
// /global/health but fails MCP usability, healing a single wedged MCP for every
// session on the serve (MCP is per-serve).
//
// Failure-tolerant: bounded by a short timeout so a single slow probe does not
// churn the watchdog into restarts (the watchdog's own backoff absorbs
// transient slowness). Returns true only on a definitive success.
func (c *SessionClient) MCPHealthy(ctx context.Context) bool {
	probeCtx, cancel := context.WithTimeout(ctx, askMCPProbeTimeout())
	defer cancel()
	return c.probeServe(probeCtx) == nil
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
	return c.SendMessageWithAttachments(ctx, sessionID, system, modelRef, text, nil)
}

// SendMessageWithAttachments appends a user message with optional image/file parts.
// Text is always first; image parts are added as base64 data URLs for vision models.
func (c *SessionClient) SendMessageWithAttachments(ctx context.Context, sessionID, system, modelRef, text string, attachments []AttachmentPart) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	parts := []map[string]any{}
	if text != "" {
		parts = append(parts, map[string]any{"type": "text", "text": text})
	}
	for _, a := range attachments {
		if a.MimeType == "" {
			a.MimeType = "application/octet-stream"
		}
		// Opencode serve accepts file/image parts as data URLs; use image type for images.
		if len(a.Data) > 0 {
			b64 := base64.StdEncoding.EncodeToString(a.Data)
			dataURL := "data:" + a.MimeType + ";base64," + b64
			if isImageMime(a.MimeType) {
				parts = append(parts, map[string]any{"type": "image", "mimeType": a.MimeType, "data": b64, "url": dataURL})
			} else {
				parts = append(parts, map[string]any{"type": "file", "mimeType": a.MimeType, "url": dataURL, "filename": a.Name})
			}
		}
	}
	if len(parts) == 0 {
		parts = append(parts, map[string]any{"type": "text", "text": text})
	}
	body := map[string]any{
		"parts": parts,
	}
	if system != "" {
		body["system"] = system
	}
	if modelRef != "" {
		// Left-greedy split: segment 1 = adapter (consumed by the control
		// plane, dropped before the serve call), segment 2 = provider,
		// remainder = model verbatim (slashes preserved — ADR-0003).
		if provider, model, ok := adapter.SplitForServe(modelRef); ok {
			body["model"] = map[string]any{"providerID": provider, "modelID": model}
		}
	}
	if err := c.do(ctx, http.MethodPost, "/session/"+sessionID+"/prompt_async", body); err != nil {
		return fmt.Errorf("opencode session send: %w", err)
	}
	return nil
}

// AttachmentPart is a single attachment forwarded to the session.
type AttachmentPart struct {
	Name     string
	MimeType string
	Data     []byte
}

func isImageMime(m string) bool {
	return len(m) >= 6 && m[:6] == "image/"
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

// Compact collapses the session's context by summarizing the collapsed
// head (the lossy compaction collapsed region), invoked when the
// execution's accumulated spend breaches its budget (soft-first, no hard
// abort). It POSTs /session/{id}/summarize with the session's RESOLVED
// provider/model so the compaction runs under the model the session
// actually uses (the acceptance criterion). The route is verified live
// (opencode 1.18.21): 200 true, then the SSE bus emits `compaction` and
// `session.compacted`. Best-effort — a failure never fails the execution.
//
// The opencode summarize contract uses camelCase keys: `providerID`,
// `modelID` (unlike SendMessage's snake_case `provider_id`).
// (the adapter segment is consumed by the control plane and dropped
// before the serve call — ADR-0003).
func (c *SessionClient) Compact(ctx context.Context, sessionID, providerID, modelID string) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	body := map[string]any{"providerID": providerID, "modelID": modelID}
	if err := c.do(ctx, http.MethodPost, "/session/"+sessionID+"/summarize", body); err != nil {
		return fmt.Errorf("opencode session compact: %w", err)
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

// Subscribe opens the serve's /event SSE stream and returns a BusSub that
// yields bus events (for ALL sessions — callers filter by session id).
// The stream blocks until ctx is cancelled or the connection drops.
func (c *SessionClient) Subscribe(ctx context.Context) (BusSub, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/event", nil, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.sseHC.Do(req)
	if err != nil {
		return nil, fmt.Errorf("opencode session subscribe: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("opencode session subscribe: http %d", resp.StatusCode)
	}
	sub := &Subscription{events: make(chan BusEvent, 1024), done: make(chan struct{}), once: make(chan struct{}), body: resp.Body}
	go sub.read(ctx, resp.Body)
	return sub, nil
}

// BusSub is the consumed surface of a /event SSE subscription: a stream of
// bus events for ALL sessions (callers filter by session id) that closes
// when the underlying connection ends or Close is called. Returned as an
// interface (not the concrete *Subscription) so consumers and tests can
// feed their own event stream without constructing the concrete type.
type BusSub interface {
	Events() <-chan BusEvent
	Done() <-chan struct{}
	Close()
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
			if evt.Type == "session.idle" {
				// session.idle is the SOLE completion signal for a turn — the event
				// a collector uses to mark a reply complete. Dropping it on a full
				// buffer would make a completed turn appear timed out (ADR-0002 D6
				// WATCH): the collector sits waiting for a completion that was
				// silently discarded, then fails that turn as a stale timeout. Never
				// drop a completion signal: deliver it, blocking only until the
				// consumer drains a slot or the subscription closes. It is rare (one
				// per turn end), so this cannot park the reader on the bus.
				select {
				case s.events <- evt:
				case <-s.once:
					return
				}
			} else {
				select {
				case s.events <- evt:
				case <-s.once:
					return
				default:
					// Bus full — DROP the event (ADR-0002 D6). The /event bus is
					// telemetry/liveness only; the durable record is the persisted
					// transcript/reply, so a dropped telemetry event never loses
					// data. Before this, a slow consumer parked its own SSE reader
					// on the full channel, stalling its connection and (with every
					// session subscribing to the one bus) affecting other sessions
					// on the same serve. Dropping makes backpressure per-session,
					// not a drive-wide stall.
				}
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

// LegacyEventFromBus maps a bus event to the legacy {type, part} event
// object the adapter's parseEvent pipeline consumes — the exact mapping
// `opencode run --format json` applies in run.ts (text only at part end,
// tool only when completed/errored, step-start/step-finish, reasoning at
// part end, and session errors). ok=false means "not a telemetry event".
// Exported so the Ask Orchicon chat turn consumes the SAME mapping
// execution sessions use — one source of truth.
func LegacyEventFromBus(evt BusEvent) (map[string]any, bool) {
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

// ToolStartFromBus reports the tool name when a bus event is the START of a
// tool call — a tool part whose state is still in flight (status "running",
// or any status other than a completed/error resolution). This is the
// tool-start signal the in-flight tool-hang watchdog arms on.
//
// It deliberately sits OUTSIDE LegacyEventFromBus, which only maps COMPLETED
// tool parts (matching run.ts: a tool is emitted only when
// completed/errored). A hung tool — one stuck at "running" forever — never
// produces a completed part, so the legacy mapping alone could never arm the
// watchdog that is supposed to interrupt exactly that hang (D6 review
// finding). Callers observe the start from the RAW bus event BEFORE the
// completed/error filter and arm the hang window; the resolution (the
// completed/error legacy event) disarms it.
//
// Returns ("", false) when the event is not a tool-start (non-tool parts,
// tool parts already resolved, or events that carry no tool name).
func ToolStartFromBus(evt BusEvent) (tool string, ok bool) {
	props := evt.Properties
	if evt.Type != "message.part.updated" {
		return "", false
	}
	part, _ := props["part"].(map[string]any)
	if part == nil {
		return "", false
	}
	ptype, _ := part["type"].(string)
	if ptype != "tool" {
		return "", false
	}
	tool, _ = part["tool"].(string)
	if tool == "" {
		return "", false
	}
	// A tool part with a completed/error status is a RESOLUTION, not a start —
	// the start event for that call (if any) already fired earlier. A part
	// without a status (or with "running"/"pending") is an in-flight start.
	status := ""
	if state, ok := part["state"].(map[string]any); ok {
		status, _ = state["status"].(string)
	}
	if status == "completed" || status == "error" {
		return "", false
	}
	return tool, true
}

// TokenDeltaInfoFromBus reports whether a bus event is a mid-generation token
// delta and returns the incremental text plus the part kind it belongs to
// ("text", "reasoning", or "" when the event does not carry a kind — callers
// treat "" as text). It accepts both the modern `message.part.delta` shape
// (the serve's text-delta / reasoning-delta stream) and the legacy
// `message.part.updated`-with-delta shape (older serves). Deltas are
// LIVENESS evidence AND the live partial-reply mirror: callers feed them to
// the progress monitors and may show them while the turn runs, but they must
// NOT be appended to the durable record — completed parts carry that, and the
// turn's finalize overwrites the partial row with the authoritative reply (a
// generation would otherwise create thousands of tiny transcript rows).
func TokenDeltaInfoFromBus(evt BusEvent) (deltaText, kind string, ok bool) {
	props := evt.Properties
	switch evt.Type {
	case "message.part.delta":
		// Modern shape: {sessionID, messageID, partID, field, delta}. The
		// serve streams text and reasoning through the SAME field "text"
		// (the field is text for both), so the kind is best-effort; other
		// fields (e.g. "metadata") are not token progress.
		field, _ := props["field"].(string)
		if field != "" && field != "text" && field != "reasoning" {
			return "", "", false
		}
		delta, _ := props["delta"].(string)
		if delta == "" {
			return "", "", false
		}
		return delta, field, true
	case "message.part.updated":
		// Legacy shape: a text/reasoning part with a `delta` subobject and
		// NO time.end (a completed part is not a delta — let the normal
		// LegacyEventFromBus path handle it).
		part, _ := props["part"].(map[string]any)
		if part == nil {
			return "", "", false
		}
		ptype, _ := part["type"].(string)
		if ptype != "text" && ptype != "reasoning" {
			return "", "", false
		}
		if tm, _ := part["time"].(map[string]any); tm != nil {
			if _, hasEnd := tm["end"]; hasEnd {
				return "", "", false
			}
		}
		delta, _ := part["delta"].(map[string]any)
		if delta == nil {
			return "", "", false
		}
		text, _ := delta["text"].(string)
		if text == "" {
			return "", "", false
		}
		return text, ptype, true
	}
	return "", "", false
}

// TokenDeltaFromBus reports whether a bus event is a mid-generation token
// delta (streamed text/reasoning) and returns the incremental text. It
// accepts both the modern `message.part.delta` shape (the serve's
// text-delta / reasoning-delta stream) and the legacy
// `message.part.updated`-with-delta shape (older serves). Deltas are
// LIVENESS evidence only — callers feed them to the progress monitors so a
// long, slow generation counts as progress; they must NOT be appended to
// output, persisted to the transcript, or streamed to the UI (completed
// parts carry the durable record, and a generation would otherwise create
// thousands of tiny transcript rows).
func TokenDeltaFromBus(evt BusEvent) (deltaText string, ok bool) {
	text, _, ok := TokenDeltaInfoFromBus(evt)
	return text, ok
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
		if resp.StatusCode == http.StatusNotFound {
			return ErrSessionNotFound
		}
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

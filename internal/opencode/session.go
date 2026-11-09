// Session transport for the opencode adapter (Stage 3).
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
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/beardedparrott/orchicon/internal/adapter"
)

// defaultServeUsername is the HTTP basic-auth username opencode serves
// with (OPENCODE_SERVER_USERNAME defaults to "opencode").
const defaultServeUsername = "opencode"

// ServePasswordEnv is the env var the host serve / runtime supervisor
// sets to protect its HTTP API with basic auth.
const ServePasswordEnv = "OPENCODE_SERVER_PASSWORD"

// PlaneSpawnEnv marks a process the PLANE itself started, and is inherited by anything that process
// goes on to spawn (the `orchicon mcp` sidecars are declared in the opencode config.
const PlaneSpawnEnv = "ORCHICON_PLANE_SPAWN"

// defaultMCPProbeTimeout bounds a single MCP-usability probe so a slow (but alive) serve is not
// treated as wedged by the watchdog and restarted in a churn.
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
	ID   string `json:"id"`
	Type string `json:"type"`
	// Properties is the v1 envelope key. v2 renamed it to Data (see Props): MEASURED against 2.0.26,
	// every event is {"id":…,"created":…,"type":…,"location":…,"data":{…},"durable":{…}}.
	Properties map[string]any `json:"properties"`
	// Data is the v2 envelope key. Both are parsed so one client reads either surface.
	Data map[string]any `json:"data"`
}

// Props returns the event's payload whichever envelope the surface used.
func (e BusEvent) Props() map[string]any {
	if e.Properties != nil {
		return e.Properties
	}
	if e.Data != nil {
		return e.Data
	}
	return nil
}

// SessionClient is an HTTP+SSE client for one opencode serve instance.
// All requests are scoped to the `directory` project path.
type SessionClient struct {
	baseURL   string
	username  string
	password  string
	directory string
	// hc is the client for short-lived API calls.
	hc *http.Client
	// sseHC is the client for long-lived /event SSE subscriptions.
	sseHC *http.Client

	// shape is WHICH API this serve speaks (see apisurface.go).
	shape apiShape

	// agentsAvailable reports whether THIS serve was configured for THIS execution, and so carries the
	// agent that holds the run-level prompt.
	agentsAvailable bool

	// log, when set, reports conditions that are otherwise invisible — currently the v2 surface's
	// missing per-message system prompt. Nil is fine: the condition simply goes unreported.
	log *slog.Logger
	// systemDroppedNoted reports ONCE that a send did not carry the prompt, at debug: on v2 that is
	// the normal case for every turn after the first.
	systemDroppedNoted atomic.Bool
	// THE DETECTION CANNOT BE A STATUS CODE.
	apiPromptOff atomic.Bool
	// apiPromptAbsentNoted reports that fallback ONCE, at warn: it is a real downgrade (the note will be
	// queued at the turn boundary rather than steered into the turn), so it is not debug noise.
	apiPromptAbsentNoted atomic.Bool

	// ONLY SESSIONS THIS CLIENT CREATED ARE IN HERE, which is what makes it safe for the paths that
	// must NOT re-inject: a follow-up ATTACHES to an existing session it did not create.
	promptPending sync.Map
}

// SetAPIShape selects the API surface this client talks to.
func (c *SessionClient) setAPIShape(shape apiShape) { c.shape = shape }

// APIShapeName reports the selected surface, for logs and for the assertions that pin wiring.
func (c *SessionClient) APIShapeName() string { return c.shape.name }

// ErrSessionNotFound marks a 404 from the serve for a session-scoped request: the session id is no
// longer valid on that serve (its data dir was wiped or the serve restarted against a fresh store).
var ErrSessionNotFound = errors.New("opencode serve: session not found")

// NewSessionClient builds a client for serve at baseURL (e.g. http://127.0.0.1:PORT).
func NewSessionClient(baseURL, password, directory string) *SessionClient {
	baseURL = strings.TrimSuffix(baseURL, "/")
	// Dedicated transport (ADR-0002 D5).
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
	// /event SSE pushes ONE long-lived stream per subscription; keep-alive is pointless and a parked
	// stream must never land in the shared idle pool.
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
		shape:     shapeV1,
	}
}

// BaseURL returns the serve base URL this client talks to.
func (c *SessionClient) BaseURL() string { return c.baseURL }

// Healthy reports whether the serve answers /global/health.
func (c *SessionClient) Healthy(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := c.newRequest(ctx, http.MethodGet, c.shape.healthPath, nil, nil)
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

// SessionExists reports whether THIS serve still holds a session id — the re-attach probe for a
// follow-up.
func (c *SessionClient) SessionExists(ctx context.Context, sessionID string) bool {
	if sessionID == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var out struct {
		ID string `json:"id"`
	}
	if err := c.doJSON(ctx, http.MethodGet, c.shape.sessionPath(url.PathEscape(sessionID)), nil, &out); err != nil {
		return false
	}
	return true
}

// probeServe exercises the serve's full readiness surface: it must answer /global/health AND accept
// a real session-create round-trip.
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

// ProbeUsable is the L1 serve-readiness probe (the workflow run-start gate): the serve must answer
// /global/health AND accept a real session-create round-trip.
func (c *SessionClient) ProbeUsable(ctx context.Context) error {
	return c.probeServe(ctx)
}

// MCPHealthy reports whether the serve's MCP-enabled agent session machinery is usable (ADR-0002 D1
// — the MCP watchdog).
func (c *SessionClient) MCPHealthy(ctx context.Context) bool {
	probeCtx, cancel := context.WithTimeout(ctx, askMCPProbeTimeout())
	defer cancel()
	return c.probeServe(probeCtx) == nil
}

// CreateSession creates a session on the serve, scoped to the client's directory.
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
	if err := c.doJSON(ctx, http.MethodPost, c.shape.sessionsPath(), body, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", fmt.Errorf("opencode session create: empty session id")
	}
	// A session this client just created has never been told the system prompt, so it is marked.
	c.promptPending.Store(out.ID, struct{}{})
	return out.ID, nil
}

// SelectAgent switches the session to a named agent.
func (c *SessionClient) SelectAgent(ctx context.Context, sessionID, agentName string) error {
	if c.shape.setAgentPath == nil || strings.TrimSpace(agentName) == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := c.do(ctx, http.MethodPost, c.shape.setAgentPath(sessionID),
		map[string]any{"agent": agentName}); err != nil {
		return fmt.Errorf("opencode session select agent: %w", err)
	}
	return nil
}

// SetAgentsAvailable records that this serve was configured for this execution and so carries the
// agent holding the run-level prompt.
func (c *SessionClient) SetAgentsAvailable(v bool) { c.agentsAvailable = v }

// AgentSelectionSupported reports whether this client can deliver a per-execution system prompt
// through an AGENT rather than through the message — BOTH conditions, not either:
func (c *SessionClient) AgentSelectionSupported() bool {
	return c.shape.setAgentPath != nil && c.agentsAvailable
}

// SendMessage appends a user message to the session via prompt_async (fire-and-forget: 204, the
// reply arrives on the SSE bus).
func (c *SessionClient) SendMessage(ctx context.Context, sessionID, system, modelRef, text string) error {
	return c.sendMessage(ctx, sessionID, system, modelRef, text, nil, false)
}

// SendMessageCarryingSystem sends a message that must DELIVER the session's system prompt on
// surfaces whose prompt body has no `system` field.
func (c *SessionClient) SendMessageCarryingSystem(ctx context.Context, sessionID, system, modelRef, text string) error {
	return c.sendMessage(ctx, sessionID, system, modelRef, text, nil, true)
}

// consumePromptPending reports whether sessionID is awaiting its system prompt, and clears the mark
// so ONE send carries it and later sends do not.
func (c *SessionClient) consumePromptPending(sessionID string) bool {
	_, pending := c.promptPending.LoadAndDelete(sessionID)
	return pending
}

// SendMessageWithAttachments appends a user message with optional file parts.
func (c *SessionClient) SendMessageWithAttachments(ctx context.Context, sessionID, system, modelRef, text string, attachments []AttachmentPart) error {
	return c.sendMessage(ctx, sessionID, system, modelRef, text, attachments, false)
}

// sendMessage is the ONE implementation.
func (c *SessionClient) sendMessage(ctx context.Context, sessionID, system, modelRef, text string, attachments []AttachmentPart, carrySystem bool) error {
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
		if len(a.Data) == 0 {
			continue
		}
		// FilePartInput: {type:"file", mime, url, filename?}.
		dataURL := "data:" + a.MimeType + ";base64," + base64.StdEncoding.EncodeToString(a.Data)
		part := map[string]any{"type": "file", "mime": a.MimeType, "url": dataURL}
		if a.Name != "" {
			part["filename"] = a.Name
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		parts = append(parts, map[string]any{"type": "text", "text": text})
	}
	// THE BODY IS BUILT FOR THE SURFACE, because the two are not interchangeable (see
	// apiShape.promptStyle).
	files := make([]map[string]any, 0, len(attachments))
	for _, a := range attachments {
		if a.MimeType == "" {
			a.MimeType = "application/octet-stream"
		}
		if len(a.Data) == 0 {
			continue
		}
		f := map[string]any{
			"uri": "data:" + a.MimeType + ";base64," + base64.StdEncoding.EncodeToString(a.Data),
		}
		if a.Name != "" {
			f["name"] = a.Name
		}
		files = append(files, f)
	}

	var body map[string]any
	switch c.shape.promptStyle {
	case "text":
		// THE PROMPT TRAVELS AS CONTENT HERE, because this surface has no field for it (see
		// apiShape.promptStyle). ONLY WHEN ASKED.
		bodyText := text
		if carrySystem && strings.TrimSpace(system) != "" {
			bodyText = system + "\n\n" + text
		}
		// AND "THE MODEL SEES IT WHEN THE TURN IS OVER".
		body = map[string]any{"text": bodyText, "delivery": "steer"}
		// FileAttachment = {uri (required), name?, description?, mention?}.
		if len(files) > 0 {
			body["files"] = files
		}
		// A caller that did NOT ask to carry the prompt is not losing it.
		if system != "" && !carrySystem {
			c.noteOnce(&c.systemDroppedNoted, "opencode v2: this send does not carry the system "+
				"prompt; it is delivered with the session's first message and re-stated after a compact "+
				"(the surface has no `system` field)")
		}
	case "nested":
		// THE 1.x STEER BODY. opencode 1.18+ carries the v2-shaped prompt route next to its v1 routes,
		// and its body nests the content under `prompt` with `delivery` as a SIBLING:
		//
		//	{"prompt": {"text": …, "files": [{uri, name}]}, "delivery": "steer"}
		//
		// MEASURED against a real 1.19.4 serve: 200 with "delivery":"steer" echoed back; files echoed
		// back; an invalid value rejected with `Expected "steer" | "queue", got "nonsense" at
		// ["delivery"]` — which is what proves the field is honoured rather than dropped.
		//
		// `text` is REQUIRED by this route (omitting it answers `Missing key at ["prompt"]["text"]`),
		// so it is always present even when empty.
		p := map[string]any{"text": text}
		if len(files) > 0 {
			p["files"] = files
		}
		body = map[string]any{"prompt": p, "delivery": "steer"}
	default:
		body = map[string]any{"parts": parts}
		if system != "" {
			body["system"] = system
		}
	}

	if modelRef != "" {
		// Left-greedy split: segment 1 = adapter (consumed by the control plane, dropped before the serve
		// call), segment 2 = provider, remainder = model verbatim (slashes preserved — ADR-0003).
		if provider, model, ok := adapter.SplitForServe(modelRef); ok {
			if c.shape.setModelPath != nil {
				// A surface whose prompt body cannot carry the model sets it on the SESSION.
				if err := c.do(ctx, http.MethodPost, c.shape.setModelPath(sessionID),
					map[string]any{"model": map[string]any{
						"providerID":           provider,
						c.shape.modelFieldName: model,
					}}); err != nil {
					return fmt.Errorf("opencode session set model: %w", err)
				}
			} else {
				body["model"] = map[string]any{"providerID": provider, "modelID": model}
			}
		}
	}
	// WHICH PROMPT ROUTE, decided with the body already built: the "nested" style is the 1.x steer route,
	// and it is only usable if this serve really has it (apiPromptOff records that it does not).
	path := c.shape.promptPath(sessionID)
	if c.shape.promptStyle == "nested" {
		if c.apiPromptOff.Load() {
			// A PREVIOUS TURN PROVED THIS SERVE HAS NO SUCH ROUTE, so the attempt is skipped entirely:
			// re-probing every turn would cost a doomed request on each one.
			return c.sendV1Prompt(ctx, sessionID, parts, system, modelRef)
		}
		admitted, err := c.postNestedPrompt(ctx, path, body)
		if err != nil {
			return fmt.Errorf("opencode session send: %w", err)
		}
		if admitted {
			return nil
		}
		// NOT THE ROUTE — see apiPromptAbsentNoted. Remember it and deliver the v1 way, so the operator's
		// message still lands (at the turn boundary) instead of being swallowed by the catch-all.
		c.apiPromptOff.Store(true)
		c.warnOnce(&c.apiPromptAbsentNoted, "opencode: this 1.x serve did not admit a prompt on the "+
			"/api prompt route (it answers unknown routes with its web app, so a 200 proves nothing) — "+
			"falling back to prompt_async. Interjections will be queued at the turn boundary on this "+
			"serve rather than steered into the running turn")
		return c.sendV1Prompt(ctx, sessionID, parts, system, modelRef)
	}
	if err := c.do(ctx, http.MethodPost, path, body); err != nil {
		return fmt.Errorf("opencode session send: %w", err)
	}
	return nil
}

// sendV1Prompt delivers a prompt the v1 way: parts + system + model in one body on the v1 route.
func (c *SessionClient) sendV1Prompt(ctx context.Context, sessionID string, parts []map[string]any, system, modelRef string) error {
	body, _ := v1PromptBody(parts, system, modelRef)
	if err := c.do(ctx, http.MethodPost, c.shape.plainPromptPath(sessionID), body); err != nil {
		return fmt.Errorf("opencode session send: %w", err)
	}
	return nil
}

// v1PromptBody is the v1 send body: `parts`, plus the system prompt and the model when there is one.
func v1PromptBody(parts []map[string]any, system, modelRef string) (map[string]any, bool) {
	body := map[string]any{"parts": parts}
	if system != "" {
		body["system"] = system
	}
	if modelRef != "" {
		if provider, model, ok := adapter.SplitForServe(modelRef); ok {
			body["model"] = map[string]any{"providerID": provider, "modelID": model}
		}
	}
	return body, true
}

// postNestedPrompt posts the 1.x steer body and reports whether the serve ADMITTED it.
func (c *SessionClient) postNestedPrompt(ctx context.Context, path string, body any) (bool, error) {
	req, err := c.newRequest(ctx, http.MethodPost, path, body, nil)
	if err != nil {
		return false, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return false, fmt.Errorf("opencode serve POST %s: http %d: %s", path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		// Not JSON: the web app, i.e. no such route on this serve.
		return false, nil
	}
	return out.Data.ID != "", nil
}

// warnOnce logs a message at WARN the first time only, so a per-turn condition does not flood the log.
func (c *SessionClient) warnOnce(flag *atomic.Bool, msg string) {
	if flag.CompareAndSwap(false, true) {
		if c.log != nil {
			c.log.Warn(msg)
		}
	}
}

// noteOnce is warnOnce at DEBUG, for a condition that is EXPECTED rather than a problem.
func (c *SessionClient) noteOnce(flag *atomic.Bool, msg string) {
	if flag.CompareAndSwap(false, true) {
		if c.log != nil {
			c.log.Debug(msg)
		}
	}
}

// AttachmentPart is a single attachment forwarded to the session.
type AttachmentPart struct {
	Name     string
	MimeType string
	Data     []byte
}

// Abort cancels the session's running turn. The session (and its history)
// is kept; a subsequent SendMessage starts a new turn.
func (c *SessionClient) Abort(ctx context.Context, sessionID string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := c.do(ctx, http.MethodPost, c.shape.abortPath(sessionID), map[string]any{}); err != nil {
		return fmt.Errorf("opencode session abort: %w", err)
	}
	return nil
}

// Compact collapses the session's context by summarizing the collapsed head (the lossy compaction
// collapsed region), invoked when the execution's accumulated spend breaches its budget.
func (c *SessionClient) Compact(ctx context.Context, sessionID, providerID, modelID string) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	body := map[string]any{"providerID": providerID, "modelID": modelID}
	if err := c.do(ctx, http.MethodPost, c.shape.compactPath(sessionID), body); err != nil {
		return fmt.Errorf("opencode session compact: %w", err)
	}
	return nil
}

// ReplyPermission answers a permission.asked event with "once" — the server-side equivalent of
// `opencode run --auto`, so tool calls never block an unattended execution.
func (c *SessionClient) ReplyPermission(ctx context.Context, sessionID, permissionID string) error {
	return c.ReplyPermissionDecision(ctx, sessionID, permissionID, "once")
}

// ReplyPermissionDecision answers a permission.asked event with an explicit serve response value
// ("once" | "reject").
func (c *SessionClient) ReplyPermissionDecision(ctx context.Context, sessionID, permissionID, decision string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := c.do(ctx, http.MethodPost, c.shape.permissionReplyPath(sessionID, permissionID),
		map[string]any{c.shape.permissionReplyField: decision}); err != nil {
		return fmt.Errorf("opencode session permission reply: %w", err)
	}
	return nil
}

// Subscribe opens the serve's /event SSE stream and returns a BusSub that yields bus events (for
// ALL sessions — callers filter by session id).
func (c *SessionClient) Subscribe(ctx context.Context) (BusSub, error) {
	req, err := c.newRequest(ctx, http.MethodGet, c.shape.eventPath(), nil, nil)
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

// BusSub is the consumed surface of a /event SSE subscription: a stream of bus events for ALL
// sessions (callers filter by session id) that closes when the underlying connection ends or Close.
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
			// THE COMPLETION SIGNAL IS NEVER DROPPED, AND v2 HAS A DIFFERENT NAME FOR IT.
			if evt.Type == "session.idle" || evt.Type == "session.execution.succeeded" || evt.Type == "session.execution.failed" {
				// session.idle is the SOLE completion signal for a turn — the event a collector uses to mark a
				// reply complete.
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
					// Bus full — DROP the event (ADR-0002 D6).
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

// LegacyEventFromBus maps a bus event to the legacy {type, part} event object the adapter's
// parseEvent pipeline consumes.
func LegacyEventFromBus(evt BusEvent) (map[string]any, bool) {
	props := evt.Props()
	switch evt.Type {
	case "session.text.ended":
		// v2's completed text block: {"data":{…,"ordinal":0,"text":"Hi!
		d := evt.Props()
		if t, _ := d["text"].(string); t != "" {
			return map[string]any{"type": "text", "part": map[string]any{"type": "text", "text": t}}, true
		}
		return nil, false
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
	// ---- v2 TELEMETRY (opencode 2.x) ---------------------------------------
	//
	// THIS BLOCK IS THE FIX for a 2.x execution being killed as `text_loop` at
	// exactly its 10-minute window while it was working. 2.x moved tools, steps,
	// reasoning and diffs OUT of `message.part.updated` and into their own event
	// family, emitted under `data`. Because none of them were classified, the
	// progress monitor's lastMeaningfulAction — advanced ONLY by tool_use,
	// step_finish and file_diff — could never advance on a 2.x serve, while text
	// deltas kept no_progress fresh. A healthy worker was therefore
	// indistinguishable from a model talking in circles, and the difference was
	// invisible because the text parts arriving made the run LOOK productive.
	//
	// Measured on execution 01M4SEJ37CZZ700HRQEPEVGV7C: three text parts reporting
	// files written and a commit pending, ZERO recorded tokens, zero tool rows in
	// the transcript, and the stall kill at 10:00.0 after dispatch.
	//
	// The names and payloads below come from the 2.x binary's own event schema
	// (session.step.*, session.tool.*, session.reasoning.*, session.diff).
	case "session.step.started":
		// A model turn beginning. v1 expressed this as a step-start PART.
		return map[string]any{"type": "step_start", "part": map[string]any{"type": "step-start"}}, true
	case "session.step.ended":
		// The turn's completion, and the ONLY carrier of token usage and cost on
		// 2.x — which is why every 2.x execution recorded 0 tokens and $0.
		return map[string]any{"type": "step_finish", "part": v2StepFinishPart(props)}, true
	case "session.step.failed":
		// A failed step is how 2.x reports a provider/model error, carrying the
		// same error the execution-level failure does.
		return map[string]any{"type": "error", "error": props["error"]}, true
	case "session.reasoning.ended":
		// The completed reasoning block. 2.x streams reasoning as its own family;
		// the deltas arrive as session.reasoning.delta (TokenDeltaInfoFromBus).
		if t, _ := props["text"].(string); t != "" {
			return map[string]any{"type": "reasoning", "part": map[string]any{"type": "reasoning", "text": t}}, true
		}
		return nil, false
	case "session.tool.success", "session.tool.failed":
		// A resolved tool call.
		return v2ToolLegacyEvent(evt.Type, props), true
	case "session.diff":
		// 2.x's structured per-file diff for a turn.
		if p := v2FirstDiffPath(props); p != "" {
			return map[string]any{"type": "file_diff", "part": map[string]any{"type": "file_diff", "path": p}}, true
		}
		return nil, false
	case "session.error":
		return map[string]any{"type": "error", "error": props["error"]}, true
	}
	return nil, false
}

// v2StepFinishPart shapes a 2.x `session.step.ended` payload into the v1 step-finish part the usage
// recorder and the budget ladder read (part.tokens / part.cost / part.reason).
func v2StepFinishPart(props map[string]any) map[string]any {
	part := map[string]any{"type": "step-finish"}
	if tokens, ok := props["tokens"]; ok && tokens != nil {
		part["tokens"] = normalizeV2StepTokens(tokens)
	}
	if cost, ok := props["cost"].(float64); ok {
		part["cost"] = cost
	}
	if finish, ok := props["finish"].(string); ok {
		part["reason"] = finish
	}
	// FILES: the paths this STEP changed, and on a Code Mode serve the ONLY per-step attribution that
	// exists.
	if files, ok := props["files"]; ok && files != nil {
		part["files"] = files
	}
	return part
}

// normalizeV2StepTokens reshapes 2.x's step token sample into the keys the consumers actually read
// — input / output / reasoning / cache{read,write} (see recordUsage.
func normalizeV2StepTokens(raw any) map[string]any {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	out := map[string]any{}
	pick := func(dst string, keys ...string) {
		for _, k := range keys {
			if v, ok := m[k]; ok {
				out[dst] = v
				return
			}
		}
	}
	pick("input", "input", "tokens_input", "prompt")
	pick("output", "output", "tokens_output", "completion")
	pick("reasoning", "reasoning", "tokens_reasoning")
	if v, ok := m["cache"]; ok {
		out["cache"] = v
	} else {
		read, hasRead := m["cache_read"]
		if !hasRead {
			read, hasRead = m["tokens_cache_read"]
		}
		write, hasWrite := m["cache_write"]
		if !hasWrite {
			write, hasWrite = m["tokens_cache_write"]
		}
		if hasRead || hasWrite {
			c := map[string]any{}
			if hasRead {
				c["read"] = read
			}
			if hasWrite {
				c["write"] = write
			}
			out["cache"] = c
		}
	}
	if len(out) == 0 {
		return m
	}
	return out
}

// v2ToolLegacyEvent shapes a resolved 2.x tool call into the v1 {"type":"tool_use","part":…} the
// transcript, the progress monitor and the repetition signature all consume.
func v2ToolLegacyEvent(evtType string, props map[string]any) map[string]any {
	status := "completed"
	if evtType == "session.tool.failed" {
		status = "error"
	}
	state := map[string]any{"status": status}
	if in, ok := props["input"]; ok && in != nil {
		state["input"] = in
	}
	if c, ok := props["content"]; ok && c != nil {
		state["output"] = c
	}
	part := map[string]any{"type": "tool_use", "callID": "", "state": state}
	if id, _ := props["id"].(string); id != "" {
		part["callID"] = id
	}
	if name, _ := props["name"].(string); name != "" {
		part["tool"] = name
	}
	return map[string]any{"type": "tool_use", "part": part}
}

// v2FirstDiffPath returns the first changed path in a 2.x `session.diff`
// payload, accepting the several field names a per-file diff may use.
func v2FirstDiffPath(props map[string]any) string {
	files, _ := props["diff"].([]any)
	for _, f := range files {
		m, _ := f.(map[string]any)
		if m == nil {
			continue
		}
		for _, k := range []string{"path", "file", "filename"} {
			if p, _ := m[k].(string); p != "" {
				return p
			}
		}
	}
	return ""
}

// ToolStartFromBus reports the tool name when a bus event is the START of a tool call — a tool part
// whose state is still in flight (status "running".
func ToolStartFromBus(evt BusEvent) (tool string, ok bool) {
	props := evt.Props()
	// ---- v2 (opencode 2.x) --------------------------------------------------
	// 2.x moved tool calls to their own event family. `session.tool.input.started`
	// carries the call's NAME and id; `session.tool.called` carries the id and the
	// resolved input but NOT the name. Either means the call is in flight, so
	// either arms the hang window — the name is only the refusal message's noun,
	// and an empty one is honest (the read path fills it in when it is known).
	if evt.Type == "session.tool.called" || evt.Type == "session.tool.input.started" {
		if name, _ := props["name"].(string); name != "" {
			return name, true
		}
		if id, _ := props["id"].(string); id != "" {
			return "", true
		}
		return "", false
	}
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
	// A tool part with a completed/error status is a RESOLUTION, not a start — the start event for that
	// call (if any) already fired earlier.
	status := ""
	if state, ok := part["state"].(map[string]any); ok {
		status, _ = state["status"].(string)
	}
	if status == "completed" || status == "error" {
		return "", false
	}
	return tool, true
}

// TokenDeltaInfoFromBus reports whether a bus event is a mid-generation token delta and returns the
// incremental text plus the part kind it belongs to ("text", "reasoning".
func TokenDeltaInfoFromBus(evt BusEvent) (deltaText, kind string, ok bool) {
	props := evt.Props()
	switch evt.Type {
	case "session.reasoning.delta":
		// 2.x streams reasoning under its OWN family (v1 folded it into message.part.delta).
		delta, _ := props["delta"].(string)
		if delta == "" {
			return "", "", false
		}
		return delta, "reasoning", true
	case "session.text.delta":
		// v2's streaming text.
		delta, _ := evt.Props()["delta"].(string)
		if delta == "" {
			return "", "", false
		}
		return delta, "text", true
	case "message.part.delta":
		// Modern shape: {sessionID, messageID, partID, field, delta}.
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
		// Legacy shape: a text/reasoning part with a `delta` subobject and NO time.end (a completed part is
		// not a delta — let the normal LegacyEventFromBus path handle it).
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

// TokenDeltaFromBus reports whether a bus event is a mid-generation token delta (streamed
// text/reasoning) and returns the incremental text.
func TokenDeltaFromBus(evt BusEvent) (deltaText string, ok bool) {
	text, _, ok := TokenDeltaInfoFromBus(evt)
	return text, ok
}

// newRequest builds an authenticated request, scoping it to the client's directory: the
// x-opencode-directory header on writes.
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
		// Surface the serve's validation message (truncated): a bare
		// "http 400" hides which part key the serve rejected.
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("opencode serve %s %s: http %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
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
		return c.decodeBody(resp.Body, out)
	}
	return nil
}

// decodeBody decodes a response body into out, unwrapping the surface's response envelope WHEN IT
// IS PRESENT and passing the body through when it is not.
func (c *SessionClient) decodeBody(r io.Reader, out any) error {
	raw, err := io.ReadAll(io.LimitReader(r, maxResponseBytes))
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil // an empty body is a valid answer for a status-only endpoint
	}
	body := raw
	if env := c.shape.responseEnvelope; env != "" {
		var wrapper map[string]json.RawMessage
		if err := json.Unmarshal(raw, &wrapper); err == nil {
			if inner, ok := wrapper[env]; ok {
				body = inner
			}
		}
	}
	if err := json.Unmarshal(body, out); err != nil {
		// Report the shape we actually got, truncated, rather than a bare unmarshal error: on a new
		// API generation the useful question is "what does it return?", and that cannot be guessed.
		return fmt.Errorf("decode %s response: %w (first %d bytes: %s)",
			c.shape.name, err, min(300, len(raw)), strings.TrimSpace(string(raw[:min(300, len(raw))])))
	}
	return nil
}

// maxResponseBytes bounds a response we will read before deciding its shape. Generous for a session
// JSON document, and small enough that a wrong endpoint cannot make us buffer something enormous.
const maxResponseBytes = 8 << 20

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

// PlaneSpawnEnv marks a process the PLANE itself started, and is inherited by
// anything that process goes on to spawn (the `orchicon mcp` sidecars are
// declared in the opencode config, so opencode starts them, not the plane —
// they carry the marker only by inheritance).
//
// It exists so that "is this process ours?" can be answered EXACTLY. Inferring
// it from parentage cannot: on a host, an `opencode` the operator started from
// their own terminal and a plane-spawned `opencode` left behind by a crash both
// end up reparented, to the same place, once their spawner exits. The marker is
// what distinguishes them, and it is deliberately NOT one of opencode's own
// variables: an operator running their own `opencode serve` may well set
// OPENCODE_SERVER_PASSWORD, so it cannot serve as proof that WE started it.
const PlaneSpawnEnv = "ORCHICON_PLANE_SPAWN"

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
	ID   string `json:"id"`
	Type string `json:"type"`
	// Properties is the v1 envelope key. v2 renamed it to Data (see Props): MEASURED against 2.0.26,
	// every event is {"id":…,"created":…,"type":…,"location":…,"data":{…},"durable":{…}}.
	Properties map[string]any `json:"properties"`
	// Data is the v2 envelope key. Both are parsed so one client reads either surface.
	Data map[string]any `json:"data"`
}

// Props returns the event's payload whichever envelope the surface used.
//
// THIS ONE METHOD IS WHY THE ASK TRANSPORT WORKS AT ALL ON v2. Every consumer read `Properties`, which
// is EMPTY on a v2 stream — so `sessionID` came back as "" for every event and each event was filtered
// out or classified as nothing. The observable effect was a turn that received no deltas, showed no
// progress, and was eventually killed by the stall monitor while the model had in fact replied and the
// reply sat in the session.
//
// v1 is preferred when both are present so a v1 serve's behaviour is byte-identical; a v2 event simply
// has no `properties` key, so the fallback is unambiguous rather than a guess.
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
	// hc is the client for short-lived API calls. It uses a DEDICATED
	// http.Transport (ADR-0002 D5) so this serve's connections never contend
	// on the package-level http.DefaultTransport shared by every SessionClient.
	hc *http.Client
	// sseHC is the client for long-lived /event SSE subscriptions. It uses a
	// separate keep-alive-disabled transport so a parked stream never occupies
	// the API idle pool (D5).
	sseHC *http.Client

	// shape is WHICH API this serve speaks (see apisurface.go). Every route below is asked of it
	// rather than written as a literal, because opencode 2.x moved them all under /api and renamed
	// several. It defaults to v1 — the surface the installed base runs — so a client that never
	// detects anything behaves exactly as it did before.
	shape apiShape

	// agentsAvailable reports whether THIS serve was configured for THIS execution, and so carries
	// the agent that holds the run-level prompt.
	//
	// IT DEFAULTS FALSE, which is the safe direction. A CONTAINER serve is built per RUN with exactly
	// that run's step workers baked in (see adapter.WorkerAgent), so the execution selects its own
	// agent there. A HOST serve is shared across projects and runs and therefore bakes NO agents —
	// its prompt travels on the message, as it always did — so selecting one would be a request for
	// an agent that cannot exist. Treating "unknown" as false keeps the host path quiet and leaves
	// the full prompt in place.
	agentsAvailable bool

	// log, when set, reports conditions that are otherwise invisible — currently the v2 surface's
	// missing per-message system prompt. Nil is fine: the condition simply goes unreported.
	log *slog.Logger
	// systemDroppedNoted reports ONCE that a send did not carry the prompt, at debug: on v2 that is
	// the normal case for every turn after the first.
	systemDroppedNoted atomic.Bool

	// promptPending holds the sessions THIS CLIENT CREATED and has not yet delivered a system prompt
	// to. It is the honest answer to "is this session new?", and the reason it is needed rather than
	// inferred: on the Ask path both the seeded (first) and the reuse (later) prompts are non-empty,
	// so `system != ""` cannot distinguish "deliver this now" from "the session already has it".
	//
	// ONLY SESSIONS THIS CLIENT CREATED ARE IN HERE, which is what makes it safe for the paths that
	// must NOT re-inject: a follow-up ATTACHES to an existing session it did not create, so it is never
	// pending and never re-states the prompt.
	promptPending sync.Map
}

// SetAPIShape selects the API surface this client talks to. It is the seam the plane uses after
// detecting the served version, and it exists as a method rather than a constructor argument so the
// many existing NewSessionClient call sites (and every test) keep compiling and keep working.
func (c *SessionClient) setAPIShape(shape apiShape) { c.shape = shape }

// APIShapeName reports the selected surface, for logs and for the assertions that pin wiring.
func (c *SessionClient) APIShapeName() string { return c.shape.name }

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

// SessionExists reports whether THIS serve still holds a session id — the
// re-attach probe for a follow-up. The transport's own store is the only
// authority: its URL changes across plane restarts while its sessions
// persist, so URL equality is not a usable proxy for continuity.
//
// Fail-closed: a blank id, an unreachable serve, a 404 (ErrSessionNotFound)
// or any other error all report false, so a follow-up seeds a FRESH session
// from the durable transcript rather than assuming a session it cannot see.
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
	if err := c.doJSON(ctx, http.MethodPost, c.shape.sessionsPath(), body, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", fmt.Errorf("opencode session create: empty session id")
	}
	// A session this client just created has never been told the system prompt, so it is marked.
	// See promptPending: the mark is what lets a surface with no `system` field deliver the prompt
	// exactly once, without every later turn re-stating it.
	c.promptPending.Store(out.ID, struct{}{})
	return out.ID, nil
}

// SelectAgent switches the session to a named agent.
//
// A NO-OP on a surface with no agent route (v1): there the worker's prompt rides
// the per-message `system` field and no agent is involved. On v2 it is load
// bearing — the agent's `system` is the only channel that governs the model, so
// without this the session runs on the serve's default agent (the worker tool
// shell) and the worker's real prompt never arrives.
//
// An empty name is a no-op rather than an error: the caller derives the name from
// a worker id it may not have, and a session left on the default agent is a
// working worker with a thinner prompt — a degraded run, not a broken one.
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

// SetAgentsAvailable records that this serve was configured for this execution and so carries
// the agent holding the run-level prompt. Called by the CONTAINER path, which builds the serve per
// run; the host path leaves it false (see the field).
func (c *SessionClient) SetAgentsAvailable(v bool) { c.agentsAvailable = v }

// AgentSelectionSupported reports whether this client can deliver a per-execution system prompt
// through an AGENT rather than through the message — BOTH conditions, not either:
//
//   - the surface must HAVE the route (v2 does; v1 has no agent at all, and there the message is
//     the only carrier), and
//   - the serve must actually carry this execution's agent (see agentsAvailable).
//
// It is the gate for the message trim, so a false answer keeps the full prompt in place. That is
// why the two conditions are combined rather than checked separately at the call site: trimming on
// a serve that has no agent would DELETE the run-level prompt instead of moving it.
func (c *SessionClient) AgentSelectionSupported() bool {
	return c.shape.setAgentPath != nil && c.agentsAvailable
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
	return c.sendMessage(ctx, sessionID, system, modelRef, text, nil, false)
}

// SendMessageCarryingSystem sends a message that must DELIVER the session's system prompt on surfaces
// whose prompt body has no `system` field.
//
// THIS IS THE ONE PLACE THE DISTINCTION LIVES, and it is a distinction the v2 surface forces. On v1
// every send carries `system` and the serve applies it per turn, so all callers pass it and none of
// them care. On v2 there is no such field, so the prompt has to travel as CONTENT — which means it
// must go in exactly ONCE per session, not on every turn, or the context grows by a full prompt copy
// per message.
//
// So callers that must deliver it say so, and every other caller keeps the old behaviour:
//   - the GOAL (the first message of a freshly created session) — carries it;
//   - the AFTER-COMPACT reminder — carries it again, because a compact summarizes the conversation
//     away and would otherwise take the prompt with it;
//   - follow-ups, redirects, retries, probes, compaction notes — do NOT, because the session already
//     holds it. The follow-up path is the one that matters: it ATTACHES to an existing session, so
//     re-sending there would add a second copy of the prompt to a conversation that already has one.
func (c *SessionClient) SendMessageCarryingSystem(ctx context.Context, sessionID, system, modelRef, text string) error {
	return c.sendMessage(ctx, sessionID, system, modelRef, text, nil, true)
}

// consumePromptPending reports whether sessionID is awaiting its system prompt, and clears the mark so
// ONE send carries it and later sends do not.
//
// This is for callers that cannot tell a first turn from a later one — the Ask path, where the seeded
// and reuse prompts are both non-empty. Callers that DO know (a session just created, an explicit
// re-statement after a compact) use SendMessageCarryingSystem and skip the bookkeeping.
func (c *SessionClient) consumePromptPending(sessionID string) bool {
	_, pending := c.promptPending.LoadAndDelete(sessionID)
	return pending
}

// SendMessageWithAttachments appends a user message with optional file
// parts. The wire shape follows the serve's prompt_async schema (vendored
// SDK ground truth:
// @opencode-ai/sdk v2 types.gen.d.ts — TextPartInput {type:"text",
// text} | FilePartInput {type:"file", mime, url, filename?}). There is
// NO "image" part type and NO mimeType/data keys: images AND text files
// alike go out as {type:"file", mime, url: dataURL, filename?} — a
// single url carrying the data: URL (never data+url together). Text files
// (.md/.txt) inline as data: URLs so the send is one POST with no
// upload-path dependency (UploadAttachment untouched).
func (c *SessionClient) SendMessageWithAttachments(ctx context.Context, sessionID, system, modelRef, text string, attachments []AttachmentPart) error {
	return c.sendMessage(ctx, sessionID, system, modelRef, text, attachments, false)
}

// sendMessage is the ONE implementation. carrySystem is meaningful only on a surface with no `system`
// field (see SendMessageCarryingSystem); on v1 it changes nothing, which is what keeps the field-based
// path byte-identical to before.
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
		// FilePartInput: {type:"file", mime, url, filename?}. The url
		// carries the data: URL (images for vision, text files
		// inline); filename is set when known.
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
	// apiShape.promptStyle). v1 takes a `parts` array plus `system` and `model`; v2 takes a
	// top-level `text` (REQUIRED) and `files`, and has no field for either of the others.
	var body map[string]any
	switch c.shape.promptStyle {
	case "text":
		// THE PROMPT TRAVELS AS CONTENT HERE, because this surface has no field for it (see
		// apiShape.promptStyle). Prepended rather than sent separately in two requests: one request is
		// atomic, so the model cannot receive the task without the instructions that frame it.
		//
		// ONLY WHEN ASKED, and that is not an optimisation — it is the correctness condition. This
		// branch is reached by EVERY send, including follow-ups onto a session that is already
		// carrying the prompt, and prepending there would append another full copy each time.
		bodyText := text
		if carrySystem && strings.TrimSpace(system) != "" {
			bodyText = system + "\n\n" + text
		}
		body = map[string]any{"text": bodyText}
		var files []map[string]any
		for _, a := range attachments {
			if a.MimeType == "" {
				a.MimeType = "application/octet-stream"
			}
			if len(a.Data) == 0 {
				continue
			}
			// FileAttachment = {uri (required), name?, description?, mention?}. v1 carried the bytes
			// as a data: URL inside a `parts` entry; v2 takes a uri, and a data: URL is a uri — so
			// the same encoding travels, under a different key and with `name` instead of `filename`.
			f := map[string]any{
				"uri": "data:" + a.MimeType + ";base64," + base64.StdEncoding.EncodeToString(a.Data),
			}
			if a.Name != "" {
				f["name"] = a.Name
			}
			files = append(files, f)
		}
		if len(files) > 0 {
			body["files"] = files
		}
		// A caller that did NOT ask to carry the prompt is not losing it: on this surface the prompt
		// reaches the model as the first message's content (SendMessageCarryingSystem) and again after
		// a compact. This is the expected case for every turn after the first, so it is not warned
		// about. It IS noted once per client at debug level, because "why is this turn's send smaller
		// than the first" is otherwise invisible.
		if system != "" && !carrySystem {
			c.noteOnce(&c.systemDroppedNoted, "opencode v2: this send does not carry the system "+
				"prompt; it is delivered with the session's first message and re-stated after a compact "+
				"(the surface has no `system` field)")
		}
	default:
		body = map[string]any{"parts": parts}
		if system != "" {
			body["system"] = system
		}
	}

	if modelRef != "" {
		// Left-greedy split: segment 1 = adapter (consumed by the control
		// plane, dropped before the serve call), segment 2 = provider,
		// remainder = model verbatim (slashes preserved — ADR-0003).
		if provider, model, ok := adapter.SplitForServe(modelRef); ok {
			if c.shape.setModelPath != nil {
				// A surface whose prompt body cannot carry the model sets it on the SESSION. Done
				// before the prompt so this turn runs on the chosen model rather than the serve's
				// default; idempotent, so re-sending for a later turn is harmless.
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
	if err := c.do(ctx, http.MethodPost, c.shape.promptPath(sessionID), body); err != nil {
		return fmt.Errorf("opencode session send: %w", err)
	}
	return nil
}

// warnOnce logs a message at WARN the first time only, so a per-turn condition does not flood the log.
func (c *SessionClient) warnOnce(flag *atomic.Bool, msg string) {
	if flag.CompareAndSwap(false, true) {
		if c.log != nil {
			c.log.Warn(msg)
		}
	}
}

// noteOnce is warnOnce at DEBUG, for a condition that is EXPECTED rather than a problem. The v2
// surface legitimately does not carry the prompt on most sends, so warning about it would train the
// operator to ignore the log line that matters.
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
	if err := c.do(ctx, http.MethodPost, c.shape.compactPath(sessionID), body); err != nil {
		return fmt.Errorf("opencode session compact: %w", err)
	}
	return nil
}

// ReplyPermission answers a permission.asked event with "once" — the
// server-side equivalent of `opencode run --auto`, so tool calls never
// block an unattended execution. It is the auto-approve shorthand; the
// consent layer answers through ReplyPermissionDecision.
func (c *SessionClient) ReplyPermission(ctx context.Context, sessionID, permissionID string) error {
	return c.ReplyPermissionDecision(ctx, sessionID, permissionID, "once")
}

// ReplyPermissionDecision answers a permission.asked event with an explicit
// serve response value ("once" | "reject").
//
// The consent core ALWAYS sends "once" or "reject": the SESSION decision
// ("allow for this directory") is kept in OUR grant store, not delegated to a
// serve-side session-scoped response value, so the grant state has exactly one
// authoritative home and works across serve builds.
func (c *SessionClient) ReplyPermissionDecision(ctx context.Context, sessionID, permissionID, decision string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := c.do(ctx, http.MethodPost, c.shape.permissionReplyPath(sessionID, permissionID),
		map[string]any{c.shape.permissionReplyField: decision}); err != nil {
		return fmt.Errorf("opencode session permission reply: %w", err)
	}
	return nil
}

// Subscribe opens the serve's /event SSE stream and returns a BusSub that
// yields bus events (for ALL sessions — callers filter by session id).
// The stream blocks until ctx is cancelled or the connection drops.
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
			// THE COMPLETION SIGNAL IS NEVER DROPPED, AND v2 HAS A DIFFERENT NAME FOR IT. This guard
			// exists so a full event buffer cannot discard the one event that ends a turn (see below);
			// v2 ends with session.execution.succeeded / .failed, so omitting them would reintroduce
			// exactly the failure the guard prevents — a completed turn waiting out its window.
			if evt.Type == "session.idle" || evt.Type == "session.execution.succeeded" || evt.Type == "session.execution.failed" {
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
	props := evt.Props()
	switch evt.Type {
	case "session.text.ended":
		// v2's completed text block: {"data":{…,"ordinal":0,"text":"Hi! …"}}. Shaped into the SAME
		// {"type":"text","part":{…}} the v1 arm produces, so every consumer downstream — the stall
		// monitor's observe, the collector's text case, the partial mirror — works unchanged.
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
	props := evt.Props()
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
	props := evt.Props()
	switch evt.Type {
	case "session.text.delta":
		// v2's streaming text. MEASURED shape:
		//   {"type":"session.text.delta","data":{"sessionID":…,"assistantMessageID":…,
		//                                         "ordinal":0,"delta":"Hi! …"}}
		// The delta is a TOP-LEVEL `delta` string (v1 nested it in {field, delta}), and v2 does not
		// distinguish text from reasoning on this event, so the kind is best-effort like v1's.
		delta, _ := evt.Props()["delta"].(string)
		if delta == "" {
			return "", "", false
		}
		return delta, "text", true
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

// decodeBody decodes a response body into out, unwrapping the surface's response envelope WHEN IT IS
// PRESENT and passing the body through when it is not.
//
// WHY THE "WHEN PRESENT" PART MATTERS, rather than unwrapping unconditionally on v2: the envelope is
// per-endpoint. Measured on 2.0.26, the session endpoints wrap ({"data":{…}}) while /api/info does not
// ({"version":…}). A decoder that assumed the wrapper would read nothing from the endpoints that omit
// it, and the failure would look exactly like the one this fixes — an empty field, no error.
//
// An absent envelope key is therefore not an error: it means this endpoint answers bare, which the
// same decoder handles. Only a body that fails BOTH readings is reported, and it is reported with the
// bytes, because a shape mismatch is not something a caller can guess at.
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

package scheduler

import (
	"context"
	"errors"

	"github.com/beardedparrott/orchicon/internal/audit"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/runtime"
)

// ExecutionManifest is the StartExecution payload sent to the adapter
// (docs/04_Runtime_Adapter_SDK.md §3.1). It contains everything the
// adapter needs to begin a WorkerExecution: identity, prompts, model,
// context sources, budgets, permissions.
type ExecutionManifest struct {
	ExecutionID   string
	TaskID        string
	ProjectID     string
	WorkerID      string
	WorkerVersion int
	SystemPrompt  string
	// PromptFingerprint is the sha256 over the project + work-item
	// context-file stamps that produced this manifest's composite prompt
	// (ADR-0009 D5). Unchanged fingerprint ⇒ the static prefix is
	// byte-identical to the previous execution's ⇒ the provider prefix
	// cache stays warm. Empty = not captured (legacy path).
	PromptFingerprint  string
	Goal               string
	AcceptanceCriteria string
	ModelRef           string // human-defined; no auto-failover (docs/05 §11)
	DefaultModelRef    string // tenant-level default from settings; fallback if ModelRef empty
	// ContextWindow is the work item's declared context window (tokens).
	// Adapters use it as a FALLBACK context-window hint when live provider
	// metadata is unavailable (e.g. a custom local provider whose models
	// report no context length) — never a replacement for a live hint.
	ContextWindow  int
	ContextSources []byte // jsonb
	Budgets        []byte // jsonb
	Permissions    []byte // jsonb
	// ProjectDir is the project root: the container bind-mount source and
	// the daemon-level guard/serve boundary. Always the true project dir.
	ProjectDir string
	// WorktreePath is the execution's working directory when the run has a
	// provisioned worktree (a subdir of ProjectDir: .orchicon-worktrees/
	// <runID>, worker starts checked out on its branch). Empty = run in
	// ProjectDir directly.
	WorktreePath string
	// RuntimeWorkflowID is the workflow run whose runtime container this
	// execution should dispatch into. When non-empty AND a runtime
	// daemon is configured, the adapter runs `opencode` inside the
	// workflow's runtime container instead of as a local subprocess.
	RuntimeWorkflowID string
	// RuntimeImage is the resolved runtime container image tag for the
	// workflow run (captured at run start). Empty = the daemon's default
	// base image.
	RuntimeImage string
	// ExecutionMode is the run's project execution_mode
	// ("runtime" | "local"). It is the adapter-side routing gate: the
	// reconciler skips EnsureForRun in "local" mode, so a local run has
	// NO container lease — an adapter must stay in-process (never exec or
	// Create a container) when this is "local", even with a reachable
	// daemon. Defaults to "runtime" when empty (legacy/standalone rows).
	ExecutionMode string
	// Stall detection thresholds from tenant settings. Zero means "use
	// env-var or built-in default".
	StallNoProgressWindowSeconds int64
	StallNoFileDiffWindowSeconds int64
	StallTextLoopWindowSeconds   int64
	StallRepetitionCount         int32
	StallRepetitionWindowSeconds int64
	// Nudge knobs (advisory-stall escalation) from tenant settings. Zero
	// means "use env-var or built-in default".
	StallNudgeMax                int32
	StallNudgeReplyWindowSeconds int64
	StallNudgeCooldownSeconds    int64

	// StallToolHangSeconds is the in-flight tool-hang watchdog window (D6):
	// a tool call with no events for longer than this is cancelled natively
	// (synthesized `cancelled:` tool result + course-correcting redirect
	// injected as the next user turn). Zero = unset (env/code default 180s);
	// negative = disabled.
	StallToolHangSeconds int64

	// SequenceContinue (opt-in, DEFAULT OFF) marks this execution as part
	// of a sequence chain: consecutive same-worker tasks may resume the
	// prior task's session transcript instead of starting a fresh session
	// (tightly-coupled chains where retained context beats isolation).
	// When true, ContinueFromSessionID names the prior session to resume.
	SequenceContinue      bool
	ContinueFromSessionID string
}

// ExecutionCallbacks are the status callbacks the adapter bridge uses to
// inform the TaskReconciler of execution lifecycle transitions
// (docs/03 §6). OnStall is the stall-detection trigger (docs/06 §2
// "stalled health state | no progress within stall window"): the adapter
// bridge's progress monitor raises it when a worker is stuck looping
// (repeated tool calls, no file changes, no token progress), and the
// TaskReconciler uses it to trigger recovery (idempotent — docs/06 §9).
type ExecutionCallbacks interface {
	OnStarted(ctx context.Context, execID string)
	// OnResult carries the worker's accumulated text output (PR B —
	// context propagation). The TaskReconciler extracts the ORCHICON
	// WORKER SUMMARY block from `output`, persists it as the work
	// item's `_summary`, and copies it onto the linked workflow step
	// run so downstream stages see it. `output` may be empty if the
	// adapter didn't accumulate any text (e.g. the worker errored
	// before producing output).
	OnResult(ctx context.Context, execID string, succeeded bool, output string, errorMessage string)
	// OnWrittenFiles carries the paths of files the worker's session
	// actually wrote or edited during the run (opencode file_diff
	// telemetry). More reliable than parsing diff markers out of the
	// worker's text output: it includes files the model saved without
	// echoing a diff (e.g. .orchicon/ review notes written via the Write
	// tool). The receiver folds these into the step run's _touched_files
	// so the next worker is told exactly what to read before it starts.
	// Called before the terminal OnResult; may be called once with the
	// full set, or incrementally.
	OnWrittenFiles(ctx context.Context, execID string, files []string)
	OnHealth(ctx context.Context, execID, healthState string)
	// OnStall signals a detected stall (the reason carries which signal
	// tripped: stalled:no_progress | stalled:no_file_progress |
	// stalled:repetition:<sig>). `fatal` distinguishes a genuine
	// hang/loop (true — the adapter has already hard-killed the
	// subprocess, so the receiver should fail the execution and trigger
	// recovery) from the advisory no_file_progress signal (false — the
	// subprocess keeps running; the receiver should surface a
	// non-terminal stalled health notice and let OnRecovered / the
	// terminal OnResult decide the outcome).
	OnStall(ctx context.Context, execID, reason string, fatal bool)
	// OnRecovered is called when an advisory stall clears — the worker
	// resumed the missing progress signal (a file_diff after a
	// no_file_progress trip) before the execution terminated. The
	// receiver flips the execution back to healthy and clears the stall
	// notice; status stays running and the terminal OnResult still
	// decides success/failure.
	OnRecovered(ctx context.Context, execID, recovered string)
	// OnToolCall notifies the runtime that the worker invoked a tool.
	// Published as a tool_call execution event for the live session pane.
	OnToolCall(ctx context.Context, execID, toolName string, input, output []byte)
	// OnText notifies the runtime that the worker produced text output.
	// Published as a telemetry execution event for the live session pane.
	OnText(ctx context.Context, execID string, text string)
	// OnArtifact notifies the runtime that the worker produced an output
	// artifact (e.g. a file via the `write` tool). The name is the file
	// path, artifactType is the MIME type or extension hint (e.g. "markdown",
	// "json", "text"), and content is the full artifact body. Published as
	// an EXECUTION_EVENT_TYPE_ARTIFACT event for inline display (docs/10 §11).
	OnArtifact(ctx context.Context, execID, name, artifactType, content string)
}

// AdapterBridge is the control-plane side of the adapter contract. It
// starts an execution on a registered adapter and streams telemetry
// back via the callbacks. The bridge abstracts whether the adapter is a
// real gRPC sidecar (v0.2+) or an in-process CLI subprocess wrapper
// (v0.1 — docs/04 §6.0).
//
// Start is the REQUIRED core capability every adapter implements. The
// rest of the server-facing surface is OPTIONAL and capability-scoped:
// MessageInjector, SessionContinuer, Aborter, LivenessReporter, and the
// construction-time ConfigurableBridge. A bridge implements the subset
// it supports; a caller that needs a capability the bridge lacks gets an
// actionable error on the corresponding RPC path — never a panic (see
// the per-interface docs and internal/server/server.go).
//
// The scheduler is the only component permitted to call Start
// (docs/03 §8 invariant #1).
type AdapterBridge interface {
	Start(ctx context.Context, exec db.ExecutionRow, manifest ExecutionManifest, callbacks ExecutionCallbacks) error
}

// MessageInjector is the optional mid-run message capability: routes a
// human message into a live session execution (execution RPC
// SendExecutionMessage). Bridges that support mid-run injection
// implement it; a bridge without it surfaces an actionable
// "adapter kind X does not support message injection" error on the RPC
// path (never a panic).
type MessageInjector interface {
	SendExecutionMessage(ctx context.Context, execID, message string) error
}

// SessionContinuer is the optional follow-up capability: runs a one-shot
// follow-up question against a worker's session in place (execution RPC
// ContinueExecutionSession). Bridges that support follow-ups implement
// it; a bridge without it surfaces an actionable error on the RPC path
// (never a panic).
type SessionContinuer interface {
	ContinueSession(ctx context.Context, opts ContinueSessionOpts) (string, error)
}

// Aborter is the optional cancellation capability: stops a live
// execution's session when a human cancels it, so the model stops
// generating immediately (workflow/execution abort RPCs). Bridges that
// support live abort implement it; a bridge without it surfaces an
// actionable error on the abort path (never a panic).
//
// AbortExecution is a safe no-op for unknown/finished executions.
type Aborter interface {
	AbortExecution(ctx context.Context, execID, reason string) error
}

// LivenessReporter is the optional liveness capability: reports whether
// an execution is still tracked as live by the bridge. Used by the
// execution reaper to detect executions orphaned by a control-plane
// restart / lost runtime container. Bridges without it are treated as
// fail-closed (not alive) by the reaper.
type LivenessReporter interface {
	IsExecutionActive(execID string) bool
}

// ErrSessionNotFound marks that a conversation's persisted session id is
// no longer known to the adapter's session transport (a serve/backend
// restart wiped its store, or the session expired). The Ask collector
// treats it as a recreate + re-seed trigger for the turn. It is the
// adapter-neutral sentinel the opencode adapter maps its transport-specific
// 404 onto, so the askorchicon service never depends on adapter types.
var ErrSessionNotFound = errors.New("adapter session not found")

// ChatTurnClient is the adapter-neutral persistent-session surface an Ask
// Orchicon conversation turn drives. It is the generalization of the
// historical askorchicon sessionTurnClient: an adapter that supports Ask
// chat implements it as an OPTIONAL capability, resolved through the shared
// Dispatcher by the conversation's model_ref adapter kind. The askorchicon
// service type-asserts it off the Dispatcher-resolved bridge and surfaces
// an actionable error when an adapter registers but does not implement it.
// Semantically identical to sessionTurnClient (Subscribe / CreateSession /
// SendMessage / Abort / ReplyPermission), scoped to a conversation.
type ChatTurnClient interface {
	// Subscribe opens an adapter-event subscription for a conversation,
	// yielding session-visible SessionEvents for the conversation's turns.
	Subscribe(ctx context.Context, conversationID string) (SessionBus, error)
	// CreateConversationSession creates a fresh session for a conversation
	// (first message, or a lost session recreated). Returns the session id.
	CreateConversationSession(ctx context.Context, conversationID, title string) (string, error)
	// SendTurnMessage appends a user message to a conversation session.
	SendTurnMessage(ctx context.Context, conversationID, sessionID, system, modelRef, text string) error
	// AbortConversationSession stops a live turn on a conversation session so
	// the model stops generating now (Stop / supersede / TTL expiry). Safe
	// no-op for unknown sessions.
	AbortConversationSession(ctx context.Context, sessionID string) error
	// ReplyPermission auto-approves a permission.asked signal.
	ReplyPermission(ctx context.Context, sessionID, permissionID string) error
}

// SessionOwner is the OPTIONAL capability for an adapter that can identify
// whether a session id belongs to it. Ask Orchicon uses it to enforce
// adapter-scoped session identity across a mid-conversation model/adapter
// switch: when the conversation's persisted session id belongs to a DIFFERENT
// adapter than the one the current model_ref resolves to, the turn must
// create a fresh session on the new adapter instead of dispatching a foreign
// session id to it (which a session-ful adapter like opencode rejects with an
// opaque 500). Adapters without this capability are treated as OWNS-ALL:
// they accept any session id (the native orchicon bridge is deliberately
// sessionless and accepts any id, so the reverse-direction opencode→native
// switch keeps working).
type SessionOwnerKind interface {
	// SessionOwnerKind returns the adapter kind this client's sessions
	// belong to (e.g. "orchicon", "opencode").
	SessionOwnerKind() string
}

// NativeSessionIDPrefix is the tag the native (orchicon) bridge prepends to
// its synthetic session ids (internal/orchicon/chatturn.go). Any session id
// carrying this prefix is owned by the native adapter; dispatching it to any
// other adapter is always a cross-adapter leak that must recreate first.
const NativeSessionIDPrefix = "orchicon-ask:"

// SendTurnMessageWithAttachments is the OPTIONAL mid-run capability for
// conversation turns that carry file/image attachments. It mirrors the
// MessageInjector capability pattern: an adapter that supports attachments
// on Ask turns implements it; an Ask turn with attachments routed to an
// adapter that lacks it FAILS LOUDLY (never silently degrading to
// text-only). The turn attaches are server-authoritative (count/size caps).
type SendTurnMessageWithAttachments interface {
	SendTurnMessageWithAttachments(ctx context.Context, conversationID, sessionID, system, modelRef, text string, attachments []ChatAttachment) error
}

// ConversationHistoryPurger is the OPTIONAL durable-history capability for the
// Ask chat path, mirroring the SendTurnMessageWithAttachments pattern.
//
// A sessionless adapter (native / "orchicon") owns a persisted history
// artifact per conversation that NOTHING else reclaims — the native bridge
// writes one JSON file per session and the conversation row is its only other
// trace — so deleting a conversation must purge it or the artifact accumulates
// forever. A session-ful adapter (opencode) keeps its history inside its own
// serve and has nothing to purge, so it simply does not implement this.
//
// The conversation-delete path type-asserts it and skips when absent: never a
// panic, never a hard failure of the delete RPC. Implementations MUST be
// idempotent — purging an unknown or never-created conversation is a
// successful no-op.
type ConversationHistoryPurger interface {
	// PurgeConversationHistory discards a conversation's durable session
	// history (in-memory and persisted). sessionID may be empty: an adapter
	// that keys history by conversation id must still resolve the artifact.
	PurgeConversationHistory(ctx context.Context, conversationID, sessionID string) error
}

// ChatAttachment is one inline attachment on an Ask turn (name, mime type,
// raw bytes) — the adapter-neutral shape of the chat attachment input
// (count/size caps are validated server-side at dispatch).
type ChatAttachment struct {
	Name     string
	MimeType string
	Data     []byte
}

// SessionEvent is one turn-visible signal an Ask drain loop consumes,
// adapter-neutral. Adapters map their own event/protocol vocabulary onto
// these turn-visible kinds; the askorchicon service and scheduler see only
// this shape, never the adapter's raw event type.
type SessionEvent struct {
	// Kind classifies the signal:
	//   "idle"       — turn complete; drain ends the turn as collected
	//   "error"      — turn failed (model/API level); Text is the message
	//   "permission" — permission.asked; PermissionID drives auto-approve
	//   "tool_part"  — a tool call issued but not yet resolved (Text = tool
	//                  name); feeds the stall monitor's wedge signal
	//   "delta"      — mid-generation token delta (Text); liveness + live mirror
	//   "part"       — a completed text/reasoning/tool_use/step_finish part
	Kind string
	// Type refines Kind for "delta" ("text"|"reasoning") and "part"
	// ("text"|"reasoning"|"tool_use"|"step_finish") — the stall-monitor and
	// session-event vocabulary.
	Type string
	// Text is the delta text ("delta"), the completed part text ("part"
	// text/reasoning), the error message ("error"), or the active tool name
	// ("tool_part"). It carries the COMMITTED folded-think body for a
	// reasoning delta produced by the adapter's think demux (see SessionBus).
	Text         string
	IsReasoning  bool
	PermissionID string
	// Part is the legacy part map for a completed "part" (a "tool_use" part
	// carries tool + args for the stall monitor's repetition signature; the
	// monitor path consumes it). Nil for other kinds.
	Part map[string]any
	// SessionID is the session the event belongs to. Adapters whose
	// transport multiplexes sessions (e.g. a shared serve bus) set it so the
	// drain loop can filter by the turn's current session id (which can
	// change across a recreation). Empty = the adapter already scoped the
	// subscription to a single session.
	SessionID string
}

// SessionBus is the adapter-neutral per-turn event surface an Ask drain loop
// consumes. It closes when the underlying transport ends or Close is called.
// The adapter owns the per-session filter and folded-think demux; the
// scheduler/askorchicon packages consume only SessionEvents.
type SessionBus interface {
	Events() <-chan SessionEvent
	Done() <-chan struct{}
	Close()
}

// ConfigurableBridge is the optional construction-time capability
// surface: the setters that inject cross-cutting wiring every adapter
// receives from the server (usage recorder, durable session transcript
// store, runtime daemon client). Declared as a single interface so
// construction stays adapter-neutral — concrete adapter types appear
// only at construction/registration, never in dispatch or callback
// paths. The server type-asserts each setter separately, so a bridge may
// implement any subset (missing setters are simply skipped).
//
// SetHostServe is deliberately NOT part of this contract: the host serve
// is opencode-specific transport plumbing (the scheduler must not import
// opencode), so the server wires it directly on the concrete adapter at
// construction — which is where concrete types are allowed.
type ConfigurableBridge interface {
	// SetUsageRecorder injects the LLM usage recorder (step_finish
	// token/cost telemetry).
	SetUsageRecorder(fn UsageRecorderFunc)
	// SetSessionStore injects the durable per-execution session
	// transcript writer.
	SetSessionStore(fn SessionStoreFunc)
	// SetRuntimeClient injects the workflow runtime daemon client.
	SetRuntimeClient(rt RuntimeClient)
}

// ContinueSessionOpts carries everything needed to run a follow-up
// question against a worker's session WITHOUT creating a new execution
// or work item. It is the adapter-neutral contract shape for the
// SessionContinuer capability — the opencode bridge implements it via
// its session transport; any future bridge accepts the same shape.
type ContinueSessionOpts struct {
	ExecutionID  string
	TenantID     string
	SystemPrompt string // the worker's composed system prompt (per-message)
	ModelRef     string
	ProjectDir   string
	Message      string // the user's follow-up question
	// Context is the durable-transcript context used to seed a FRESH
	// session when the original serve/session is no longer reachable.
	Context string
	// Original session identity (from the transcript's session_info part);
	// re-attached when the serve is still reachable for real continuity.
	SessionID     string
	ServeURL      string
	ServePassword string
	// WorkerID is the caller's execution worker. The bridge uses it to
	// enforce identity isolation by comparing it against the prior
	// transcript's identity block (a follow-up must belong to the same
	// worker as the original run — no worker ever sees another worker's
	// transcript). Empty (bridge-level tests) skips the worker check.
	WorkerID string
	// StartSeq is the next transcript seq to use (the last existing part's
	// seq + 1), so the follow-up entries append after the original run.
	StartSeq int64

	// The following reconstruct the worker session so a follow-up runs as a
	// FULL live session (same MCP + host tools, same context, same agent
	// loop) rather than a bare one-shot. The execution service populates
	// them from the execution row + work item; the opencode adapter ignores
	// them (its follow-up re-attaches to the serve).
	WorktreePath       string
	RuntimeImage       string
	RuntimeWorkflowID  string
	ExecutionMode      string
	WorkerName         string
	ProjectID          string
	TaskID             string
	Goal               string // the ORIGINAL execution goal (context, not the follow-up question)
	AcceptanceCriteria string
	ContextWindow      int64
}

// UsageRecord is the usage sample a bridge emits on step_finish (docs/04
// §6.1 step_finish carries tokens + cost). It is the adapter-neutral
// shape onto the canonical aigateway.UsageInput — the server copies it
// field-for-field into the gateway's input so the gateway never branches
// on adapter/provider.
type UsageRecord struct {
	TenantID         string
	ProjectID        string
	TaskID           string
	ExecutionID      string
	WorkerID         string
	Provider         string
	Model            string
	PromptTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	CompletionTokens int64
	ReasoningTokens  int64
	CostUSD          float64
	CorrelationID    string
	TraceID          string
	WorkflowRunID    string // immutable link to the workflow run; survives execution deletion
}

// UsageRecorderFunc records a usage sample. Decoupled from the
// aigateway package via a function type so the scheduler and adapters
// have no import dependency on the gateway.
type UsageRecorderFunc func(ctx context.Context, in UsageRecord) error

// SessionStoreFunc persists transcript entries for one execution. The
// implementation owns the tenant transaction.
type SessionStoreFunc func(ctx context.Context, execID, tenantID string, parts []db.SessionPart) error

// RuntimeClient is the workflow runtime daemon client the server injects
// into adapters via ConfigurableBridge. Satisfied by *runtime.Client
// (internal/runtime imports neither scheduler nor opencode, so the
// contract stays acyclic). A nil value means the daemon is absent
// (headless serve) and the adapter stays in-process.
type RuntimeClient interface {
	// Create ensures the runtime container for a workflow run exists and
	// returns its serve endpoint.
	Create(ctx context.Context, req runtime.CreateRequest) (*runtime.CreateResponse, error)
	// Ready reports whether the runtime daemon is reachable.
	Ready(ctx context.Context) bool
	// Kill tears down a workflow run's runtime container.
	Kill(ctx context.Context, workflowID string) error
	// Exec runs one shell command inside the run's leased runtime
	// container (always-container native transport). A nil client means
	// the daemon is absent (headless/local) and the adapter stays
	// in-process.
	Exec(ctx context.Context, workflowID string, req runtime.ExecRequest) (*runtime.ExecResult, error)
	// Images lists the daemon's stock runtime images.
	Images(ctx context.Context) (*runtime.ImageList, error)
}

// Compile-time proof that the production runtime daemon client satisfies
// the contract shape (so a future adapter can rely on the interface
// without importing runtime itself).
var _ RuntimeClient = (*runtime.Client)(nil)

// TaskReconciler implements ExecutionCallbacks so the adapter bridge can
// notify it of lifecycle transitions without import cycles.
var _ ExecutionCallbacks = (*TaskReconciler)(nil)

// RecoveryTrigger is the interface the TaskReconciler uses to trigger
// recovery when an execution fails (docs/06 §2). Satisfied by the
// recovery.Engine; declared here to avoid a scheduler→recovery import
// (loose coupling).
//
// stepRunID is the workflow step run the failed execution belongs to
// ("" for standalone, non-workflow failures). Recovery is scoped per
// failing step run so every step that fails gets its own recovery cycle,
// and the work item (the ticket) stays untouched during a run.
//
// auditEntry, when non-nil, is recorded atomically with the recovery
// creation inside the engine's own transaction (the RPC path passes the
// resolved actor; the reconciler system path passes nil).
type RecoveryTrigger interface {
	TriggerOnFailure(ctx context.Context, tenantID, taskID, failedExecID, stepRunID, triggerReason string, auditEntry *audit.Entry) error
}

// PolicyEvaluator is the interface the WorkflowReconciler uses to
// evaluate gate policies (docs/02 §2.5 Tier 1). Satisfied by the
// policy.Engine; declared here to avoid a scheduler→policy import for
// the reconciler (the engine is still constructed in the server and
// injected).
type PolicyEvaluator interface {
	// EvaluateGate returns (allowed, error). allowed=false blocks the
	// step transition (docs/02 §2.5: gate denied → blocked).
	EvaluateGate(ctx context.Context, tenantID, gatePolicyRef, targetType, targetID string, input any) (bool, error)
}

// TaskDispatcher is the interface the WorkflowReconciler uses to
// dispatch a ready work item to a WorkerExecution immediately after
// creating it (docs/03 §8 invariant #1: only the TaskReconciler creates
// WorkerExecutions). The WorkflowReconciler calls DispatchTask after
// its own transaction commits so the work item is visible to the
// TaskReconciler's dispatch transaction.
//
// stepRunID is the workflow step run this dispatch is for ("" for
// standalone dispatch). Dispatch is scoped to the step run — the work
// item is a shared input reference, so multiple steps bound to the same
// item each get their own execution without mutating the item.
type TaskDispatcher interface {
	DispatchTask(ctx context.Context, taskID, stepRunID string) error
}

package scheduler

import (
	"context"
	"errors"

	"github.com/beardedparrott/orchicon/internal/audit"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/runtime"
)

// ExecutionManifest is the StartExecution payload sent to the adapter
// (docs/04_Runtime_Adapter_SDK.md §3.1).
type ExecutionManifest struct {
	ExecutionID   string
	TaskID        string
	ProjectID     string
	WorkerID      string
	WorkerVersion int
	// AgentPromptLen is the BYTE LENGTH of the run-level prefix of SystemPrompt — the shared stable
	// prefix plus the worker's identity block (db.AgentSystemPrompt).
	AgentPromptLen int
	SystemPrompt   string
	// PromptFingerprint is the sha256 over the project + work-item context-file stamps that produced
	// this manifest's composite prompt.
	PromptFingerprint  string
	Goal               string
	AcceptanceCriteria string
	ModelRef           string // human-defined; no auto-failover (docs/05 §11)
	DefaultModelRef    string // tenant-level default from settings; fallback if ModelRef empty
	// ContextWindow is the work item's declared context window (tokens).
	ContextWindow  int
	ContextSources []byte // jsonb
	Budgets        []byte // jsonb
	Permissions    []byte // jsonb
	// ProjectDir is the project root: the container bind-mount source and
	// the daemon-level guard/serve boundary. Always the true project dir.
	ProjectDir string
	// WorktreePath is the execution's working directory when the run has a provisioned worktree (a
	// subdir of ProjectDir: .orchicon-worktrees/ <runID>, worker starts checked out on its branch).
	WorktreePath string
	// RuntimeWorkflowID is the workflow run whose runtime container this execution should dispatch
	// into.
	RuntimeWorkflowID string
	// RuntimeImage is the resolved runtime container image tag for the workflow run (captured at run
	// start).
	RuntimeImage string
	// ExecutionMode is the run's project execution_mode ("runtime" | "local").
	ExecutionMode string
	// Stall detection thresholds from tenant settings.
	StallNoProgressWindowSeconds *int64
	StallNoFileDiffWindowSeconds *int64
	StallTextLoopWindowSeconds   *int64
	StallRepetitionCount         *int32
	StallRepetitionWindowSeconds *int64
	// Nudge knobs (advisory-stall escalation) from tenant settings. Same
	// convention: nil = blank (adapter default), non-nil = explicit, 0 = disabled.
	StallNudgeMax                *int32
	StallNudgeReplyWindowSeconds *int64
	StallNudgeCooldownSeconds    *int64

	// StallToolHangSeconds is the in-flight tool-hang watchdog window (D6): a tool call with no events
	// for longer than this is cancelled natively (synthesized `cancelled:` tool result +.
	StallToolHangSeconds *int64

	// SequenceContinue (opt-in, DEFAULT OFF) marks this execution as part of a sequence chain.
	SequenceContinue      bool
	ContinueFromSessionID string
}

// ExecutionCallbacks are the status callbacks the adapter bridge uses to inform the TaskReconciler
// of execution lifecycle transitions (docs/03 §6).
type ExecutionCallbacks interface {
	OnStarted(ctx context.Context, execID string)
	// OnResult carries the worker's accumulated text output (PR B — context propagation).
	OnResult(ctx context.Context, execID string, succeeded bool, output string, errorMessage string)
	// OnWrittenFiles carries the paths of files the worker's session actually wrote or edited during
	// the run (opencode file_diff telemetry).
	OnWrittenFiles(ctx context.Context, execID string, files []string)
	OnHealth(ctx context.Context, execID, healthState string)
	// OnStall signals a detected stall (the reason carries which signal tripped: stalled:no_progress |
	// stalled:no_file_progress | stalled:repetition:<sig>).
	OnStall(ctx context.Context, execID, reason string, fatal bool)
	// OnRecovered is called when an advisory stall clears — the worker resumed the missing progress
	// signal (a file_diff after a no_file_progress trip) before the execution terminated.
	OnRecovered(ctx context.Context, execID, recovered string)
	// OnToolCall notifies the runtime that the worker invoked a tool.
	// Published as a tool_call execution event for the live session pane.
	OnToolCall(ctx context.Context, execID, toolName string, input, output []byte)
	// OnText notifies the runtime that the worker produced text output.
	// Published as a telemetry execution event for the live session pane.
	OnText(ctx context.Context, execID string, text string)
	// OnArtifact notifies the runtime that the worker produced an output artifact (e.g. a file via the
	// `write` tool).
	OnArtifact(ctx context.Context, execID, name, artifactType, content string)
}

// AdapterBridge is the control-plane side of the adapter contract. Start is the REQUIRED core
// capability every adapter implements.
type AdapterBridge interface {
	Start(ctx context.Context, exec db.ExecutionRow, manifest ExecutionManifest, callbacks ExecutionCallbacks) error
}

// MessageInjector is the optional mid-run message capability: routes a human message into a live
// session execution (execution RPC SendExecutionMessage).
type MessageInjector interface {
	SendExecutionMessage(ctx context.Context, execID, message string) error
}

// SessionContinuer is the optional follow-up capability: runs a one-shot follow-up question against
// a worker's session in place (execution RPC ContinueExecutionSession).
type SessionContinuer interface {
	ContinueSession(ctx context.Context, opts ContinueSessionOpts) (string, error)
}

// ContextCompacter is the OPTIONAL context-compaction capability: asks a live execution's session
// to compact its working context in place.
type ContextCompacter interface {
	CompactExecution(ctx context.Context, execID, provider, model, remainingScope string) error
}

// Aborter is the optional cancellation capability: stops a live execution's session when a human
// cancels it, so the model stops generating immediately (workflow/execution abort RPCs).
type Aborter interface {
	AbortExecution(ctx context.Context, execID, reason string) error
}

// LivenessReporter is the optional liveness capability: reports whether an execution is still
// tracked as live by the bridge.
type LivenessReporter interface {
	IsExecutionActive(execID string) bool
}

// DiffLedgerSink is the OPTIONAL capability for a bridge that can report the file edits its
// sessions make.
type DiffLedgerSink interface {
	// SetDiffLedger installs the ledger callback for worker executions (owner kind execution) and for
	// Ask conversations (owner kind ask_conversation).
	SetDiffLedger(execution, ask DiffLedgerFunc)
}

// DiffLedgerFunc is the ledger callback shape.
type DiffLedgerFunc func(ctx context.Context, ownerID, tenantID, execDir, toolName string, input map[string]any, output string)

// ErrSessionNotFound marks that a conversation's persisted session id is no longer known to the
// adapter's session transport (a serve/backend restart wiped its store, or the session expired).
var ErrSessionNotFound = errors.New("adapter session not found")

// ChatTurnClient is the adapter-neutral persistent-session surface an Ask Orchicon conversation
// turn drives.
type ChatTurnClient interface {
	// Subscribe opens an adapter-event subscription for a conversation,
	// yielding session-visible SessionEvents for the conversation's turns.
	Subscribe(ctx context.Context, conversationID string) (SessionBus, error)
	// CreateConversationSession creates a fresh session for a conversation
	// (first message, or a lost session recreated). Returns the session id.
	CreateConversationSession(ctx context.Context, conversationID, title string) (string, error)
	// SendTurnMessage appends a user message to a conversation session.
	SendTurnMessage(ctx context.Context, conversationID, sessionID, system, modelRef, text string) error
	// AbortConversationSession stops a live turn on a conversation session so the model stops
	// generating now (Stop / supersede / TTL expiry).
	AbortConversationSession(ctx context.Context, sessionID string) error
	// ReplyPermission auto-approves a permission.asked signal.
	ReplyPermission(ctx context.Context, sessionID, permissionID string) error
	// ReplyPermissionDecision answers a permission.asked signal with an explicit serve response value
	// ("once" | "reject").
	ReplyPermissionDecision(ctx context.Context, sessionID, permissionID, decision string) error
}

// SessionQueuesPrompts is implemented by chat adapters that can HOLD a message sent while a turn is
// running and fold it into that SAME turn — rather than starting a competing one.
type SessionQueuesPrompts interface {
	// QueuesPrompts reports whether SendTurnMessage on a live session folds the message into the
	// CURRENT turn (true) rather than starting its own (false).
	QueuesPrompts() bool
}

// SessionOwner is the OPTIONAL capability for an adapter that can identify whether a session id
// belongs to it.
type SessionOwnerKind interface {
	// SessionOwnerKind returns the adapter kind this client's sessions
	// belong to (e.g. "orchicon", "opencode").
	SessionOwnerKind() string
}

// InProcessToolRunner is the OPTIONAL capability for an adapter that EXECUTES the model's tool
// calls itself, in this process, rather than handing them to a session serve.
type InProcessToolRunner interface {
	// ToolsRunInProcess reports whether every tool call this transport emits is executed by the
	// transport itself.
	ToolsRunInProcess() bool
}

// NativeSessionIDPrefix is the tag the native (orchicon) bridge prepends to its synthetic session
// ids (internal/orchicon/chatturn.go).
const NativeSessionIDPrefix = "orchicon-ask:"

// SendTurnMessageWithAttachments is the OPTIONAL mid-run capability for conversation turns that
// carry file/image attachments.
type SendTurnMessageWithAttachments interface {
	SendTurnMessageWithAttachments(ctx context.Context, conversationID, sessionID, system, modelRef, text string, attachments []ChatAttachment) error
}

// Implementations MUST be idempotent — purging an unknown or never-created conversation is a
// successful no-op.
type ConversationHistoryPurger interface {
	// PurgeConversationHistory discards a conversation's durable session history (in-memory and
	// persisted).
	PurgeConversationHistory(ctx context.Context, conversationID, sessionID string) error
}

// ChatCompaction is the outcome of compacting an Ask conversation's context.
type ChatCompaction struct {
	// Compacted is false when nothing was done — an empty history, or a history already small enough
	// that a lossy collapse would cost detail for no benefit.
	Compacted bool
	// Detail is a one-line, user-facing explanation of the outcome.
	Detail string
	// Summary is the summary text that replaced the collapsed history. Empty
	// when Compacted is false.
	Summary string
	// TokensBefore / TokensAfter are the conversation's MEASURED prompt size around the compaction.
	TokensBefore int64
	TokensAfter  int64
	// ArchivePath is where the PRE-collapse history was preserved, when the adapter keeps one.
	ArchivePath string
}

// AskCompactNotice describes a compaction that replaced a conversation's accumulated context.
type AskCompactNotice struct {
	ConversationID string
	SessionID      string
	// Reason is why the compaction ran: "pressure" (the proactive context-pressure gate),
	// "context_limit" (reactive recovery on an overflow), or "manual" (a client's /compact).
	Reason string
	// Detail is the adapter's one-line, user-facing outcome, e.g. "compacted 2343 messages into 1
	// summary + 7 recent messages (transcript reduced from 9.3MB to 412.8KB before summarizing)".
	Detail string
	// ArchivePath is where the pre-collapse history was preserved, when it was.
	ArchivePath string
	// TokensBefore / TokensAfter are the measured prompt sizes around the
	// compaction; 0 means unknown, never an estimate.
	TokensBefore int64
	TokensAfter  int64
}

// AskCompactNoticeFunc records a completed compaction.
type AskCompactNoticeFunc func(ctx context.Context, notice AskCompactNotice) error

// CompactConversationOpts identifies the conversation to compact.
type CompactConversationOpts struct {
	ConversationID string
	// SessionID is the conversation's session on its adapter. Empty is legal:
	// a sessionless adapter derives its synthetic id from the conversation id.
	SessionID string
	ModelRef  string
	// Reason is why compaction was requested: "manual", "pressure", or
	// "reactive". Recorded verbatim in the audit trail.
	Reason string
}

// ChatCompactor is the OPTIONAL context-compaction capability for the Ask chat path, mirroring the
// SendTurnMessageWithAttachments pattern.
type ChatCompactor interface {
	CompactConversationSession(ctx context.Context, opts CompactConversationOpts) (ChatCompaction, error)
}

// ChatAttachment is one inline attachment on an Ask turn (name, mime type, raw bytes) — the
// adapter-neutral shape of the chat attachment input (count/size caps are validated server-side.
type ChatAttachment struct {
	Name     string
	MimeType string
	Data     []byte
}

// ToolPolicy is the resolved tool restriction for ONE Ask turn: the mode it applies to, and the
// tool names that mode may not run.
type ToolPolicy struct {
	Mode   string
	Denied []string
}

// An adapter that does NOT implement this is dispatching a turn where the mode boundary is PROSE
// ONLY.
type ChatToolRestrictor interface {
	RestrictChatTools(ctx context.Context, policy ToolPolicy) error
}

// SessionEvent is one turn-visible signal an Ask drain loop consumes, adapter-neutral.
type SessionEvent struct {
	// Kind classifies the signal: "idle" — turn complete.
	Kind string
	// Type refines Kind for "delta" ("text"|"reasoning") and "part"
	// ("text"|"reasoning"|"tool_use"|"step_finish") — the stall-monitor and session-event vocabulary.
	Type string
	// Text is the delta text ("delta"), the completed part text ("part" text/reasoning), the error
	// message ("error"), or the active tool name ("tool_part").
	Text         string
	IsReasoning  bool
	PermissionID string
	// Detail carries the raw transport properties of a "permission" event (opencode emits id,
	// sessionID, permission/title, patterns, metadata and callID).
	Detail map[string]any
	// Part is the legacy part map for a completed "part" (a "tool_use" part carries tool + args for the
	// stall monitor's repetition signature; the monitor path consumes it).
	Part map[string]any
	// --- Typed tool-resolution fields (Kind "tool_result") ---
	ToolCallID string
	ToolName   string
	ArgsJSON   string
	Output     string
	IsError    bool
	// --- Typed consent fields (Kind "permission") ---
	Tool      string
	Command   string
	Targets   []string
	InputJSON string
	Directory string
	// --- Typed question fields (Kind "question") ---
	Question   string
	Options    []string
	AllowOther bool
	// SessionID is the session the event belongs to.
	SessionID string
}

// SessionBus is the adapter-neutral per-turn event surface an Ask drain loop consumes.
type SessionBus interface {
	Events() <-chan SessionEvent
	Done() <-chan struct{}
	Close()
}

// ConfigurableBridge is the optional construction-time capability surface: the setters that inject
// cross-cutting wiring every adapter receives from the server (usage recorder.
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

// ContinueSessionOpts carries everything needed to run a follow-up question against a worker's
// session WITHOUT creating a new execution or work item.
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
	// Original session identity (from the transcript's session_info part).
	SessionID     string
	ServeURL      string
	ServePassword string
	// AdapterKind is the adapter identity recorded in the transcript's session_info part
	// (adapter.KindOpencode / adapter.KindOrchicon).
	AdapterKind string
	// WorkerID is the caller's execution worker.
	WorkerID string
	// StartSeq is the next transcript seq to use (the last existing part's
	// seq + 1), so the follow-up entries append after the original run.
	StartSeq int64

	// The following reconstruct the worker session so a follow-up runs as a FULL live session (same MCP
	// + host tools, same context, same agent loop) rather than a bare one-shot.
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

// UsageRecord is the usage sample a bridge emits on step_finish (docs/04 §6.1 step_finish carries
// tokens + cost).
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
	// AdapterKind tags the adapter that drove the model call (persisted as usage_records.adapter_kind).
	AdapterKind string
	// SessionID carries the Ask Orchicon conversation id for Ask attribution — a chat turn has no
	// execution/task/project row to attribute through.
	SessionID string
}

// UsageRecorderFunc records a usage sample.
type UsageRecorderFunc func(ctx context.Context, in UsageRecord) error

// SessionStoreFunc persists transcript entries for one execution. The
// implementation owns the tenant transaction.
type SessionStoreFunc func(ctx context.Context, execID, tenantID string, parts []db.SessionPart) error

// RuntimeClient is the workflow runtime daemon client the server injects into adapters via
// ConfigurableBridge.
type RuntimeClient interface {
	// Create ensures the runtime container for a workflow run exists and
	// returns its serve endpoint.
	Create(ctx context.Context, req runtime.CreateRequest) (*runtime.CreateResponse, error)
	// Ready reports whether the runtime daemon is reachable.
	Ready(ctx context.Context) bool
	// Kill tears down a workflow run's runtime container.
	Kill(ctx context.Context, workflowID string) error
	// Exec runs one shell command inside the run's leased runtime container (always-container native
	// transport).
	Exec(ctx context.Context, workflowID string, req runtime.ExecRequest) (*runtime.ExecResult, error)
	// Images lists the daemon's stock runtime images.
	Images(ctx context.Context) (*runtime.ImageList, error)
}

// Compile-time proof that the production runtime daemon client satisfies the contract shape (so a
// future adapter can rely on the interface without importing runtime itself).
var _ RuntimeClient = (*runtime.Client)(nil)

// TaskReconciler implements ExecutionCallbacks so the adapter bridge can
// notify it of lifecycle transitions without import cycles.
var _ ExecutionCallbacks = (*TaskReconciler)(nil)

// RecoveryTrigger is the interface the TaskReconciler uses to trigger recovery when an execution
// fails (docs/06 §2).
type RecoveryTrigger interface {
	TriggerOnFailure(ctx context.Context, tenantID, taskID, failedExecID, stepRunID, triggerReason string, auditEntry *audit.Entry) error
}

// PolicyEvaluator is the interface the WorkflowReconciler uses to evaluate gate policies (docs/02
// §2.5 Tier 1).
type PolicyEvaluator interface {
	// EvaluateGate returns (allowed, error). allowed=false blocks the
	// step transition (docs/02 §2.5: gate denied → blocked).
	EvaluateGate(ctx context.Context, tenantID, gatePolicyRef, targetType, targetID string, input any) (bool, error)
}

// TaskDispatcher is the interface the WorkflowReconciler uses to dispatch a ready work item to a
// WorkerExecution immediately after creating it (docs/03 §8 invariant #1.
type TaskDispatcher interface {
	DispatchTask(ctx context.Context, taskID, stepRunID string) error
}

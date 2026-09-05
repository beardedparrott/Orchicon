package orchicon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/agentmemory"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/opencode"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// ErrPanic is returned by Session.Run when a panic inside the agent loop
// (tool execution, provider callback, transcript append) is recovered at
// the session boundary. The execution fails with the panic captured in
// its error output; the transcript is marked failed; the control-plane
// process and sibling executions survive.
var ErrPanic = errors.New("session panic recovered")

// Identity is the session's identity block (isolation by construction).
// One session per worker execution: session id == execution id. The
// identity carries the worker's name/purpose + this task's description/
// acceptance criteria so no worker ever sees another worker's transcript.
type Identity struct {
	ExecutionID        string
	TenantID           string
	ProjectID          string
	TaskID             string
	WorkerID           string
	WorkerName         string
	Purpose            string
	ModelRef           string // orchicon/<provider>/<model> (verbatim)
	ProviderID         string // parsed segment 2
	Model              string // parsed remainder (verbatim)
	SystemPrompt       string // manifest.SystemPrompt (assembled composite)
	Goal               string // manifest.Goal
	AcceptanceCriteria string // manifest.AcceptanceCriteria
}

// ToolRegistry is the tool-execution surface the loop consumes. The
// tool-suite task provides the real implementation; tests inject a mock.
// The loop is written against this interface so a signature tweak is
// localized.
type ToolRegistry interface {
	// Defs returns the tool definitions advertised to the model each turn.
	Defs() []ToolDef
	// Execute runs one tool call and returns the result as JSON. The
	// result is capped by the loop before it re-enters history.
	Execute(ctx context.Context, name, argsJSON string) (resultJSON string, err error)
}

// Session is ONE worker execution's agent session: identity, provider
// bound once at session start, the tool registry, and the crash-safe
// transcript. It runs the agent turn loop (loop.go) and is the unit of
// resume/continuation (the JSONL transcript is replayable).
type Session struct {
	id         string // session id == execution id
	identity   Identity
	provider   Provider     // resolved once at session start (no per-turn model switch)
	tools      ToolRegistry // injected (tool-suite task / tests); may be nil
	transcript *JSONLTranscript
	dir        string // session transcript directory (<project_dir>/.orchicon/sessions)
	log        *slog.Logger
	// history is the replayable conversation (user/assistant/tool messages).
	history []Message
	// output accumulates the session's assistant text (OnResult parity).
	output *strings.Builder
	// pendingObserver holds a transcript observer set before the
	// transcript exists (SetTranscriptObserver pre-Run); OpenTranscript
	// applies it.
	pendingObserver func(seq int64, typ string, data []byte)
	// inj is the mid-run injection queue (nil until first inject).
	inj *injected
	// continuationPath is the prior session's transcript path when this
	// session resumes a sequence chain (sequence-continuation flag,
	// opt-in default off). Seeded into the transcript at Run time.
	continuationPath string
	// continued records that the transcript was seeded from a prior
	// session (goal-append gate: a continued session carries its own new
	// goal as a user message after the seeded history).
	continued bool
	// completionProbesSent counts the completion-probe interjections this
	// session has sent (decision-signal guard, completion.go). Bounded by
	// completionProbeMaxTurns — a session that cannot deliver its
	// ORCHICON WORKER SUMMARY sign-off within the budget fails honestly
	// instead of recording a hollow success.
	completionProbesSent int
	// maxStepsVal is the resolved per-execution turn budget
	// (budgets.max_steps → ORCHICON_SESSION_MAX_STEPS → 25; loop.go).
	// Resolved once at construction — constant within a run.
	maxStepsVal int
	// lengthContinuationsSent counts the output-cap continuation turns
	// (StopLength recovery, loop.go). Bounded by
	// lengthContinuationMaxTurns — a session that keeps hitting the cap
	// past the budget fails honestly instead of looping forever.
	lengthContinuationsSent int
	// written-tracking (deduped, OnWrittenFiles parity).
	writtenMu    sync.Mutex
	writtenSet   map[string]bool
	writtenPaths []string
	// envFacts is the per-run environment-facts block, rendered once at
	// construction from manifest fields (constant within a run, so it
	// lives INSIDE the cached static prefix — ADR-0009 D2).
	envFacts string
	// prefixFingerprint is the sha256 (hex, first 16) of the composite
	// prompt bytes this session's static prefix starts from — logged on
	// change (a prefix change costs a cache miss, ADR-0009 D5).
	prefixFingerprint string
	// mutable session-zone state (ADR-0009 D2): memory notes (insertion
	// order) and the latest todowrite payload. Guarded by noteMu; never
	// read while building the static prefix.
	noteMu      sync.Mutex
	notes       []string
	latestTodos []byte
	// projectDir is the TRUE project directory (manifest.ProjectDir) — the
	// durable store location (memory.db / offload) is derived from it, NOT
	// the per-run worktree path (worktrees are pruned per step; memory must
	// survive).
	projectDir string
	// memStore is the durable agent-memory store (D2); nil = memory tools
	// answer with a clear unavailable error.
	memStore *agentmemory.Store
	// mp is the session's memory policy (enabled + digest cap), parsed from
	// the merged settings JSON.
	mp MemoryPolicy
	// cp is the session's compaction policy, parsed from the merged
	// settings JSON.
	cp CompactPolicy
	// cs is the guarded-compaction latch + shared budget state (D1).
	cs compactState
	// startedAt anchors the wall-clock dimension of the budget ladder.
	startedAt time.Time
	// toolUses counts executed tool calls (live, for the tools dimension).
	toolUses int
	// window cache: resolved once per session (never per-turn probed). The
	// resolved ModelInfo (live ListModels result) carries the context hint
	// AND the pricing used to price each turn's usage for the budget cost
	// dimension — one live resolution serves both.
	windowMu       sync.Mutex
	windowResolved bool
	windowHint     ContextWindowHint
	windowModel    *ModelInfo
	// cacheStats accumulates per-session prefix-cache metrics (ADR-0009
	// D6): cache hit/miss classification per turn + cached tokens.
	cacheMu    sync.Mutex
	cacheStats CacheStats

	// usageSink drains one live provider-turn Usage for per-record emission
	// (D2, opencode step_finish parity). Nil by default — the bridge wires
	// it only when a usage recorder is configured. It is INDEPENDENT of
	// recordTurnUsage / cacheStats: emitting a record must never double-feed
	// the per-session cache rollup.
	usageSink func(ctx context.Context, u Usage)

	// pm is the progress monitor (opencode parity — internal/orchicon/
	// progress.go): time-based stall detection (no_progress / no_file_diff /
	// text_loop / repetition / tool_hang) with the advisory-first nudge
	// escalation. Started by Run, stopped at session end.
	pm *progressMonitor
	// stallCh carries fatal stall reasons from the monitor to the loop's
	// select (the loop unwinds; the verdict was already delivered by the
	// monitor handler — fireTerminalOnce — so the loop never re-fires).
	stallCh chan string
	// doneCh is closed when Run unwinds; the nudge reply watchdog listens
	// so it never outlives the session.
	doneCh chan struct{}
	// terminalFired guards the ONE terminal OnResult per session (the
	// monitor goroutine and the turn loop both own terminal paths —
	// opencode parity: finish() is first-arrival-wins).
	terminalMu    sync.Mutex
	terminalFired bool
	// nudgePending tracks an unanswered nudge (reply-window watchdog);
	// nudgeFinished latches session end.
	nudgePending  bool
	nudgeFinished bool
	// nudge knobs (opencode parity): manifest (tenant settings) first,
	// env fallback. Resolved once at construction so the nudge budget is
	// stable for the session's lifetime.
	nudgeMaxVal         int
	nudgeReplyWindowVal time.Duration
	nudgeCooldownVal    time.Duration
	// nudgesSent counts the escalating liveness nudges this session sent
	// (advisory-stall path); lastNudgeAt is the last nudge time (cooldown).
	nudgesSent  int
	lastNudgeAt time.Time
	// contextWindowFallback is the work item's configured context window
	// (ExecutionManifest.ContextWindow). Used as the compaction hint ONLY
	// when the live provider resolution fails, so an operator-declared
	// window still arms window-pressure math instead of leaving the
	// trigger permanently disarmed. 0 = none.
	contextWindowFallback int64
}

// CacheStats is the session's prefix-cache metric rollup (ADR-0009 D6).
// Turns counts provider turns; Hits counts turns the provider reported a
// cache read on (hit), MissWrites turns that wrote cache (miss → write),
// NoneTurns turns with neither. Cache tokens accumulate from per-turn
// Usage. PrefixFingerprint identifies the static-prefix bytes the stats
// were measured against.
type CacheStats struct {
	Turns             int64
	Hits              int64
	MissWrites        int64
	NoneTurns         int64
	InputTokens       int64
	OutputTokens      int64
	ReasoningTokens   int64
	CacheReadTokens   int64
	CacheWriteTokens  int64
	PrefixFingerprint string
}

// SessionConfig carries the session's construction inputs.
type SessionConfig struct {
	ExecRow    db.ExecutionRow // ExecutionRow used for identity fields
	Manifest   scheduler.ExecutionManifest
	ProjectDir string           // the true project dir (manifest.ProjectDir)
	Provider   Provider         // pre-resolved provider (tests); may be nil → resolve via resolver
	Resolver   ProviderResolver // resolves provider/model from manifest.ModelRef
	Tools      ToolRegistry     // may be nil
	Log        *slog.Logger
	// MemoryStore is the durable agent-memory store (D2); nil = memory
	// tools answer with a clear unavailable error.
	MemoryStore *agentmemory.Store
}

// ProviderResolver resolves the provider for a (tenantID, providerID)
// pair. Satisfied by *orchicon.Registry.
type ProviderResolver interface {
	Get(ctx context.Context, tenantID, providerID string) (Provider, error)
}

// NewSession constructs a session for one execution. The provider is
// resolved once here (model bound at session start — no per-turn model
// switch). Sessions are per-execution; the JSONL path embeds the
// execution id. `projectDir` is the daemon-level serve/guard boundary:
// the transcript lives at <projectDir>/.orchicon/sessions/<id>.jsonl.
func NewSession(cfg SessionConfig) (*Session, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	projectDir := cfg.ProjectDir
	if projectDir == "" {
		projectDir = cfg.Manifest.ProjectDir
	}
	// Parse the model ref: orchicon/<provider>/<model> (verbatim model
	// remainder, internal slashes preserved — ADR-0003).
	providerID, model, ok := adapter.SplitForServe(cfg.Manifest.ModelRef)
	if !ok || providerID == "" {
		return nil, fmt.Errorf("session: model ref %q has no provider/model (expected orchicon/<provider>/<model>)", cfg.Manifest.ModelRef)
	}
	var prov Provider
	if cfg.Provider != nil {
		prov = cfg.Provider
	} else {
		if cfg.Resolver == nil {
			return nil, fmt.Errorf("session: no provider resolver configured for provider %q", providerID)
		}
		var err error
		prov, err = cfg.Resolver.Get(context.Background(), cfg.ExecRow.TenantID, providerID)
		if err != nil {
			return nil, fmt.Errorf("session: resolve provider %q: %w", providerID, err)
		}
	}
	ident := Identity{
		ExecutionID:        cfg.ExecRow.ID,
		TenantID:           cfg.ExecRow.TenantID,
		ProjectID:          cfg.ExecRow.ProjectID,
		TaskID:             cfg.ExecRow.TaskID,
		WorkerID:           cfg.ExecRow.WorkerID,
		WorkerName:         cfg.ExecRow.WorkerName,
		Purpose:            cfg.Manifest.Goal, // worker purpose == this task's goal (identity isolation)
		ModelRef:           cfg.Manifest.ModelRef,
		ProviderID:         providerID,
		Model:              model,
		SystemPrompt:       cfg.Manifest.SystemPrompt,
		Goal:               cfg.Manifest.Goal,
		AcceptanceCriteria: cfg.Manifest.AcceptanceCriteria,
	}
	s := &Session{
		id:         cfg.ExecRow.ID,
		identity:   ident,
		provider:   prov,
		tools:      cfg.Tools,
		dir:        filepath.Join(projectDir, ".orchicon", "sessions"),
		log:        cfg.Log,
		output:     &strings.Builder{},
		projectDir: projectDir,
		memStore:   cfg.MemoryStore,
		startedAt:  time.Now(),
	}
	s.mp = DefaultMemoryPolicy()
	s.cp = DefaultCompactPolicy()
	// Parse the merged settings JSON (context_compaction/memory keys ride
	// the existing budget transport — D4).
	if len(cfg.Manifest.Budgets) > 0 {
		s.cp, s.mp = policyFromSettings(cfg.Manifest.Budgets)
		s.cs.budget = opencode.ParseBudgetLadder(cfg.Manifest.Budgets)
		s.cs.spend = opencode.NewBudgetSpend()
	}
	// Resolve the turn budget from the same merged payload (worker
	// budgets.max_steps over tenant defaults, then env, then 25).
	s.maxStepsVal = maxStepsFromBudgets(cfg.Manifest.Budgets)
	// Progress monitor (opencode parity): stall windows from the manifest
	// (tenant settings) with env fallback. Started by Run.
	s.pm = newProgressMonitor(cfg.ExecRow.ID, stallWindowsFromManifest(
		cfg.Manifest.StallNoProgressWindowSeconds,
		cfg.Manifest.StallNoFileDiffWindowSeconds,
		cfg.Manifest.StallTextLoopWindowSeconds,
		cfg.Manifest.StallRepetitionCount,
		cfg.Manifest.StallRepetitionWindowSeconds,
		cfg.Manifest.StallToolHangSeconds,
	))
	s.stallCh = make(chan string, 1)
	s.doneCh = make(chan struct{})
	// Nudge knobs: manifest value first, env fallback (opencode parity).
	s.nudgeMaxVal = nudgeMaxFromManifest(cfg.Manifest.StallNudgeMax)
	s.nudgeReplyWindowVal = nudgeReplyWindowFromManifest(cfg.Manifest.StallNudgeReplyWindowSeconds)
	s.nudgeCooldownVal = nudgeCooldownFromManifest(cfg.Manifest.StallNudgeCooldownSeconds)
	// Work-item-declared context window (parity input): used only as the
	// compaction hint fallback when the live provider resolution fails.
	s.contextWindowFallback = int64(cfg.Manifest.ContextWindow)
	// Static-prefix env facts (ADR-0009 D2): rendered once at
	// construction from manifest fields — constant within a run, so the
	// cached prefix is byte-identical across turns.
	s.envFacts = envFactsBlock(envFactsFields{
		ProjectDir:   cfg.Manifest.ProjectDir,
		WorktreePath: cfg.Manifest.WorktreePath,
		RuntimeImage: cfg.Manifest.RuntimeImage,
		ModelRef:     cfg.Manifest.ModelRef,
	})
	s.prefixFingerprint = fingerprintPrefix(ident.SystemPrompt)
	return s, nil
}

// ID returns the session id (== execution id).
func (s *Session) ID() string { return s.id }

// Identity returns the session identity block.
func (s *Session) Identity() Identity { return s.identity }

// Provider returns the bound provider (resolved once at session start).
func (s *Session) Provider() Provider { return s.provider }

// TranscriptPath returns the JSONL transcript path for this session.
func (s *Session) TranscriptPath() string {
	return filepath.Join(s.dir, s.id+".jsonl")
}

// Transcript returns the session's transcript handle (nil before the
// first OpenTranscript — Run opens it). The bridge sets its DB-mirroring
// observer via SetTranscriptObserver BEFORE Run; the observer is applied
// the moment the transcript opens, so no first-turn events are missed.
func (s *Session) Transcript() *JSONLTranscript {
	return s.transcript
}

// SetTranscriptObserver installs the transcript observer that the
// session applies the instant its transcript opens in Run (before any
// event is appended). Calling it after the transcript is already open
// applies it immediately.
func (s *Session) SetTranscriptObserver(fn func(seq int64, typ string, data []byte)) {
	if t := s.transcript; t != nil {
		t.SetObserver(fn)
		return
	}
	s.pendingObserver = fn
}

// OpenTranscript opens (or reopens, for resume) the session's JSONL in
// append mode. Caller must Close it.
func (s *Session) OpenTranscript() (*JSONLTranscript, error) {
	t, err := openTranscript(s.TranscriptPath())
	if err != nil {
		return nil, err
	}
	if s.pendingObserver != nil && t.observer == nil {
		t.SetObserver(s.pendingObserver)
		s.pendingObserver = nil
	}
	s.transcript = t
	return t, nil
}

// History returns the current replayable history.
func (s *Session) History() []Message { return s.history }

// SetHistory replaces the history (used by resume/continuation replay).
func (s *Session) SetHistory(h []Message) { s.history = h }

// SetContinuation marks this session as continuing a prior session's
// transcript (sequence-continuation flag, opt-in default off). The prior
// transcript must belong to the SAME worker (identity isolation) — the
// bridge verifies this before calling; a mismatch is refused and the
// session starts fresh. path is the prior session's JSONL path.
func (s *Session) SetContinuation(path string) {
	s.continuationPath = path
}

// SetUsageSink registers a per-turn usage drain (D2, opencode step_finish
// parity). The loop calls it once per provider turn with the LIVE
// provider-reported usage (after pricing). It is a SEPARATE drain from
// recordTurnUsage — the per-session CacheStats rollup never depends on
// whether a per-record usage is emitted.
func (s *Session) SetUsageSink(fn func(ctx context.Context, u Usage)) {
	s.usageSink = fn
}

// AddMemoryNote persists one durable note into the session's mutable
// zone (orchicon_memory_note tool). Bounded: the oldest notes drop past
// maxMemoryNotes so the after-breakpoint block stays small.
func (s *Session) AddMemoryNote(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	s.noteMu.Lock()
	defer s.noteMu.Unlock()
	s.notes = append(s.notes, text)
	if len(s.notes) > maxMemoryNotes {
		s.notes = s.notes[len(s.notes)-maxMemoryNotes:]
	}
}

// MemoryNotes returns the session's memory notes in insertion order.
func (s *Session) MemoryNotes() []string {
	s.noteMu.Lock()
	defer s.noteMu.Unlock()
	return append([]string(nil), s.notes...)
}

// CacheStats returns the session's prefix-cache metric rollup.
func (s *Session) CacheStats() CacheStats {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	out := s.cacheStats
	out.PrefixFingerprint = s.prefixFingerprint
	return out
}

// recordTurnUsage folds one provider turn's usage into the session's
// cache stats. The static-prefix fingerprint is exposed via
// CacheStats.PrefixFingerprint so an operator can correlate a miss with
// a prefix edit (ADR-0009 D5/D6); the per-path prefix-change logging
// lives at the composite-build layer (scheduler prompt-section cache).
func (s *Session) recordTurnUsage(u Usage) {
	s.cacheMu.Lock()
	s.cacheStats.Turns++
	s.cacheStats.InputTokens += u.InputTokens
	s.cacheStats.OutputTokens += u.OutputTokens
	s.cacheStats.ReasoningTokens += u.ReasoningTokens
	s.cacheStats.CacheReadTokens += u.CacheReadTokens
	s.cacheStats.CacheWriteTokens += u.CacheWriteTokens
	switch {
	case u.CacheReadTokens > 0:
		s.cacheStats.Hits++
	case u.CacheWriteTokens > 0:
		s.cacheStats.MissWrites++
	default:
		s.cacheStats.NoneTurns++
	}
	s.cacheMu.Unlock()
}

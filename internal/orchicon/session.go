package orchicon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/db"
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
	// written-tracking (deduped, OnWrittenFiles parity).
	writtenMu    sync.Mutex
	writtenSet   map[string]bool
	writtenPaths []string
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
		id:       cfg.ExecRow.ID,
		identity: ident,
		provider: prov,
		tools:    cfg.Tools,
		dir:      filepath.Join(cfg.ProjectDir, ".orchicon", "sessions"),
		log:      cfg.Log,
		output:   &strings.Builder{},
	}
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

// OpenTranscript opens (or reopens, for resume) the session's JSONL in
// append mode. Caller must Close it.
func (s *Session) OpenTranscript() (*JSONLTranscript, error) {
	t, err := openTranscript(s.TranscriptPath())
	if err != nil {
		return nil, err
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

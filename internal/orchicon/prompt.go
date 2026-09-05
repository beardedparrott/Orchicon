package orchicon

// Native prompt assembly (ADR-0009): the composite prompt built by the
// scheduler (stable prefix + worker identity + task/AC + project &
// work-item context + instructions) is consumed VERBATIM as the cached
// static prefix. The native layer adds only a thin authoritative block
// (≤ 2 KiB, four topics) and a mutable zone that NEVER precedes the
// cache breakpoint:
//
//	[STATIC PREFIX: composite + NativeStaticLayer + env facts]  Cache:true
//	→ cache breakpoint
//	[MUTABLE ZONE: memory notes + latest todo digest]            Cache:false
//	→ [APPEND-ONLY HISTORY] (TurnRequest.Messages)
//
// Anthropic gets an explicit cache_control breakpoint on the flagged
// blocks only (see buildAnthropicRequest); OpenAI-compatible providers
// get byte-stable prefix ordering, which yields implicit prefix caching.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
)

// NativeStaticLayer is the thin authoritative native block appended to
// the composite prompt INSIDE the cached static prefix. It is a
// constant so identical inputs produce byte-identical prefix bytes.
// Covers exactly four topics and nothing more (the native-layer cap
// test guards against prompt creep): identity authority, tool
// discipline, todo/memory usage, environment facts (the dynamic
// per-run part of the environment facts is rendered by envFactsBlock
// from manifest fields — still constant within a run).
const NativeStaticLayer = `## Native Session Contract (Orchicon)

### Identity authority
You are an Orchicon worker executing one assigned work item inside a
native in-process session. Your role, task, and acceptance criteria are
in this prompt above — they are authoritative. There is no human on this
session; work autonomously to completion and report via the ORCHICON
WORKER SUMMARY contract given above.

### Tool discipline
Turn economy: every tool round-trip costs one turn against your
max_steps budget — 30 file operations as 30 single calls dies at the
cap, as one batched turn each it costs a handful. If you need 2+ files
or 2+ searches, you MUST use batch_read / batch_grep / batch_write so
they ride ONE tool call, not N calls over N turns (batch_read with
["a.go", "b.go"], never two read calls).
Never re-read a file whose content is already in your context — every
extra call re-sends the whole conversation. Prefer one turn carrying
several calls over several turns of one call each.

### Todo & memory usage
Maintain the live todo list with todowrite (full replacement array) at
every turn boundary; todoread re-syncs it cheaply. Persist durable
facts, root causes, and decisions with the memory note tool
(orchicon_memory_note) so later steps inherit them; keep transient
scratch in-session only.`

// maxMemoryNotes bounds the session memory-note store (the mutable zone
// stays bounded; oldest notes are dropped past the cap).
const maxMemoryNotes = 64

// todoDigestArgs is the tolerant shape of a todowrite call's arguments:
// {"todos":[{"content":...,"status":...,"priority":...}]}. Mirrors the
// post-hoc parser in internal/execution/todos.go so the digest and the
// UI's todo view agree on one payload shape.
type todoDigestArgs struct {
	Todos []struct {
		Content  string `json:"content"`
		Status   string `json:"status"`
		Priority any    `json:"priority"`
	} `json:"todos"`
}

// fingerprintPrefix is the sha256 (first 16 hex chars) of the static
// prefix bytes — logged on change so an operator can correlate a cache
// miss with a prefix edit (ADR-0009 D5).
func fingerprintPrefix(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// AssembleSystem returns the session's two-zone system block list for a
// turn request (ADR-0009 D2): block 0 is the static prefix (composite +
// native layer + env facts) carrying the cache breakpoint; block 1 (when
// non-empty) is the mutable zone (memory notes + latest todo digest) and
// is NEVER cache-flagged. Deterministic given identical session state.
func (s *Session) AssembleSystem() []SystemBlock {
	blocks := make([]SystemBlock, 0, 2)
	static := s.identity.SystemPrompt + "\n\n" + NativeStaticLayer
	if s.envFacts != "" {
		static += "\n\n" + s.envFacts
	}
	blocks = append(blocks, SystemBlock{Text: static, Cache: true})
	if zone := s.renderMutableZone(); zone != "" {
		blocks = append(blocks, SystemBlock{Text: zone, Cache: false})
	}
	return blocks
}

// renderMutableZone renders the after-breakpoint mutable block from live
// session state: durable-memory digest (D3), memory notes (insertion
// order) and the latest todo digest (array order). Returns "" when there
// is no mutable state, in which case AssembleSystem emits the single
// cached block only.
func (s *Session) renderMutableZone() string {
	notes := s.MemoryNotes()
	todos := s.TodosDigest()
	digest := s.memoryDigest()
	if len(notes) == 0 && todos == "" && digest == "" {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("## Session State (mutable — after the cache breakpoint)\n")
	if digest != "" {
		// The durable-memory digest is capped (titles+tags only, <=1 KiB)
		// and injected AFTER the cache breakpoint — the static prefix
		// bytes are untouched (D3 / prefix-stability contract).
		sb.WriteString("\n")
		sb.WriteString(digest)
	}
	if len(notes) > 0 {
		sb.WriteString("\n### Memory notes\n")
		for _, n := range notes {
			fmt.Fprintf(&sb, "- %s\n", n)
		}
	}
	if todos != "" {
		sb.WriteString("\n### Current todo list\n")
		sb.WriteString(todos)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// TodosDigest renders the latest todowrite payload as a stable
// 1-line-per-item digest. Malformed payloads degrade to "" (the zone
// must never carry nondeterministic bytes).
func (s *Session) TodosDigest() string {
	s.noteMu.Lock()
	raw := s.latestTodos
	s.noteMu.Unlock()
	if len(raw) == 0 {
		return ""
	}
	var d todoDigestArgs
	if err := json.Unmarshal(raw, &d); err != nil || d.Todos == nil {
		return ""
	}
	var sb strings.Builder
	for _, t := range d.Todos {
		prio := ""
		switch v := t.Priority.(type) {
		case float64:
			prio = fmt.Sprintf("%d", int64(v))
		case string:
			prio = v
		}
		if prio != "" {
			fmt.Fprintf(&sb, "- [%s] %s (priority %s)\n", t.Status, t.Content, prio)
		} else {
			fmt.Fprintf(&sb, "- [%s] %s\n", t.Status, t.Content)
		}
	}
	return sb.String()
}

// envFactsFields are the per-run manifest fields rendered into the
// environment-facts block (constant within a run).
type envFactsFields struct {
	ProjectDir   string
	WorktreePath string
	RuntimeImage string
	ModelRef     string
}

// envFactsBlock renders the per-run environment facts that are constant
// within a run (identical inputs → identical bytes, so they belong in
// the cached static prefix). Built once at session construction from
// manifest fields.
func envFactsBlock(manifestFields envFactsFields) string {
	image := manifestFields.RuntimeImage
	if image == "" {
		image = "base"
	}
	worktree := manifestFields.WorktreePath
	if worktree == "" {
		worktree = "none — working directly in the project directory"
	}
	return "### Environment facts\n\n" +
		"- OS/arch: " + runtime.GOOS + "/" + runtime.GOARCH + "\n" +
		"- Project directory: " + manifestFields.ProjectDir + "\n" +
		"- Worktree (working dir): " + worktree + "\n" +
		"- Runtime image: " + image + "\n" +
		"- Model: " + manifestFields.ModelRef
}

// memoryNoteToolDef is the native session's memory tool: registered by
// the loop itself (session-scoped, never the MCP registry) so the model
// can persist durable notes into the mutable zone.
func memoryNoteToolDef() ToolDef {
	return ToolDef{
		Name:        "orchicon_memory_note",
		Description: "Persist one durable fact / root cause / decision into this session's memory notes (rendered into the session-state digest after the cache breakpoint, replayed on resume). For facts later steps must inherit — not transient scratch.",
		ParamsJSON:  `{"type":"object","properties":{"text":{"type":"string","description":"The note to persist (one fact, root cause, or decision)."}},"required":["text"]}`,
	}
}

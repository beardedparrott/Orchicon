package acp

// updates.go — THE TYPED `session/update` SUM TYPE.
//
// Every variant agy 1.5.13 was recorded emitting is decoded into its own Go type. Two variants
// are modelled although agy never sends them: `agent_thought_chunk` (docs 3.1 — reasoning text
// is not exposed over ACP) and `plan`, so a future agy that does send them needs no client
// change.
//
// AN UNKNOWN OR UNDECODABLE UPDATE IS NOT AN ERROR. It becomes Unknown and the session carries
// on — the same forward compatibility agy itself practises ("ignoring unsupported event").
// Failing a turn because a future agy added a field would be strictly worse than dropping a
// display-only update.

import (
	"encoding/json"
	"log/slog"
)

// sessionUpdate kinds (the `sessionUpdate` discriminator values).
const (
	KindAgentMessageChunk  = "agent_message_chunk"
	KindAgentThoughtChunk  = "agent_thought_chunk"
	KindUserMessageChunk   = "user_message_chunk"
	KindToolCall           = "tool_call"
	KindToolCallUpdate     = "tool_call_update"
	KindPlan               = "plan"
	KindUsageUpdate        = "usage_update"
	KindAvailableCommands  = "available_commands_update"
	KindCurrentModeUpdate  = "current_mode_update"
	KindConfigOptionUpdate = "config_option_update"
	KindSessionInfoUpdate  = "session_info_update"
)

// Tool call statuses agy uses.
const (
	StatusPending    = "pending"
	StatusInProgress = "in_progress"
	StatusCompleted  = "completed"
	StatusFailed     = "failed"
)

// Update is one decoded `session/update` notification.
//
// The interface is sealed by the unexported isUpdate method: the set of variants is the set
// decodeUpdate can produce, so a consumer's type switch over it is exhaustive.
type Update interface {
	// Kind is the sessionUpdate discriminator agy sent.
	Kind() string
	// SessionID is the session the update belongs to.
	SessionID() string
	// Raw is the update object exactly as agy sent it, for fields this package does not model.
	Raw() json.RawMessage
	isUpdate()
}

// Base carries the fields every update shares. Embed it; do not use it directly.
type Base struct {
	SID      string          `json:"-"`
	RawBytes json.RawMessage `json:"-"`
}

// NewBase builds the embedded part of an update. Exported so a synthetic update can be
// constructed in a test without going through the wire.
func NewBase(sessionID string, raw json.RawMessage) Base {
	return Base{SID: sessionID, RawBytes: raw}
}

// SessionID implements Update.
func (b Base) SessionID() string { return b.SID }

// Raw implements Update.
func (b Base) Raw() json.RawMessage { return b.RawBytes }

// isUpdate seals the Update interface to this package.
func (b Base) isUpdate() {}

// AgentMessageChunk is streamed assistant text.
type AgentMessageChunk struct {
	Base
	Content ContentBlock
}

func (AgentMessageChunk) Kind() string { return KindAgentMessageChunk }

// AgentThoughtChunk is streamed reasoning text. agy 1.5.13 never emits it (docs 3.1), but the
// task requires the type and a future agy may expose it.
type AgentThoughtChunk struct {
	Base
	Content ContentBlock
}

func (AgentThoughtChunk) Kind() string { return KindAgentThoughtChunk }

// UserMessageChunk is a replayed user turn, seen on `session/load`.
type UserMessageChunk struct {
	Base
	Content ContentBlock
}

func (UserMessageChunk) Kind() string { return KindUserMessageChunk }

// Location is one path a tool call touched.
type Location struct {
	Path string `json:"path"`
}

// ToolContent is one entry of a tool call's `content[]`. agy emits
// `{"type":"content","content":{"type":"text","text":...}}` for command output and
// `{"type":"diff","path":...,"newText":...}` for edits.
type ToolContent struct {
	Type    string          `json:"type"`
	Path    string          `json:"path,omitempty"`
	OldText string          `json:"oldText,omitempty"`
	NewText string          `json:"newText,omitempty"`
	Content *ContentBlock   `json:"content,omitempty"`
	Raw     json.RawMessage `json:"-"`
}

// UnmarshalJSON keeps the raw form alongside the decoded fields.
func (t *ToolContent) UnmarshalJSON(b []byte) error {
	type alias ToolContent
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*t = ToolContent(a)
	t.Raw = append(json.RawMessage(nil), b...)
	return nil
}

// Text returns the text of a `content` item, or "" for any other kind.
func (t ToolContent) Text() string {
	if t.Content == nil {
		return ""
	}
	return t.Content.Text
}

// ToolCallInfo is the tool-call payload shared by `tool_call` and `tool_call_update`.
type ToolCallInfo struct {
	ToolCallID string          `json:"toolCallId"`
	Name       string          `json:"name"`
	Title      string          `json:"title"`
	Kind       string          `json:"kind"`
	Status     string          `json:"status"`
	Args       json.RawMessage `json:"args"`
	RawInput   json.RawMessage `json:"rawInput"`
	RawOutput  json.RawMessage `json:"rawOutput"`
	Locations  []Location      `json:"locations"`
	Content    []ToolContent   `json:"content"`
}

// ToolCall is a new tool call (the first sighting of a toolCallId).
type ToolCall struct {
	Base
	ToolCallInfo
}

func (ToolCall) Kind() string { return KindToolCall }

// ToolCallUpdate is a later state of an existing tool call, including diff content.
type ToolCallUpdate struct {
	Base
	ToolCallInfo
}

func (ToolCallUpdate) Kind() string { return KindToolCallUpdate }

// PlanEntry is one step of a plan.
type PlanEntry struct {
	Content  string `json:"content"`
	Priority string `json:"priority"`
	Status   string `json:"status"`
}

// Plan is a plan update. Modelled from ACP; agy 1.5.13 was not recorded sending one.
type Plan struct {
	Base
	Entries []PlanEntry
}

func (Plan) Kind() string { return KindPlan }

// UsageUpdate is the context-window gauge agy emits around a turn (`used` of `size`).
type UsageUpdate struct {
	Base
	Used int64 `json:"used"`
	Size int64 `json:"size"`
	// Per-turn token fields are decoded if a future agy puts them here; today they arrive on the
	// prompt RESULT (PromptResult.Usage), not on an update.
	InputTokens   *int64 `json:"inputTokens,omitempty"`
	OutputTokens  *int64 `json:"outputTokens,omitempty"`
	ThoughtTokens *int64 `json:"thoughtTokens,omitempty"`
	TotalTokens   *int64 `json:"totalTokens,omitempty"`
}

func (UsageUpdate) Kind() string { return KindUsageUpdate }

// Command is one slash command agy offers.
type Command struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// AvailableCommands replaces the advertised slash-command list (agy repeats it every turn).
type AvailableCommands struct {
	Base
	Commands []Command
}

func (AvailableCommands) Kind() string { return KindAvailableCommands }

// CurrentModeUpdate reports the session's mode (default / accept-edits / auto / plan).
type CurrentModeUpdate struct {
	Base
	ModeID string
}

func (CurrentModeUpdate) Kind() string { return KindCurrentModeUpdate }

// ConfigOptionUpdate carries the current config options (model family, thought level, mode).
type ConfigOptionUpdate struct {
	Base
	Options json.RawMessage
}

func (ConfigOptionUpdate) Kind() string { return KindConfigOptionUpdate }

// SessionInfoUpdate carries the conversation title agy derives from the first prompt.
type SessionInfoUpdate struct {
	Base
	Title string
}

func (SessionInfoUpdate) Kind() string { return KindSessionInfoUpdate }

// Unknown is an update this package does not model, or one whose payload did not decode. It is
// logged at debug level and otherwise ignored; the session continues.
type Unknown struct {
	Base
	// UpdateKind is the sessionUpdate value agy sent ("" when the object had no
	// discriminator at all).
	UpdateKind string
	// Err is the decode error, when the kind is known but the payload did not match.
	Err error
}

// Kind implements Update. It is a method here (while the value lives in UpdateKind) so a caller
// can distinguish "agy invented a kind" from "agy changed a known one".
func (u Unknown) Kind() string { return u.UpdateKind }

// updateHeader is the discriminator-only view of an update object.
type updateHeader struct {
	SessionUpdate string `json:"sessionUpdate"`
}

// decodeUpdate turns one update object into an Update. It NEVER fails: anything it cannot model
// comes back as Unknown.
func decodeUpdate(sessionID string, raw json.RawMessage) Update {
	base := Base{SID: sessionID, RawBytes: append(json.RawMessage(nil), raw...)}

	var hdr updateHeader
	if err := json.Unmarshal(raw, &hdr); err != nil {
		slog.Default().Debug("acp: undecodable session/update", "session", sessionID, "err", err)
		return Unknown{Base: base, Err: err}
	}

	unknown := func(err error) Update {
		if err == nil {
			slog.Default().Debug("acp: ignoring unsupported session/update",
				"session", sessionID, "kind", hdr.SessionUpdate)
		} else {
			slog.Default().Debug("acp: session/update payload did not decode",
				"session", sessionID, "kind", hdr.SessionUpdate, "err", err)
		}
		return Unknown{Base: base, UpdateKind: hdr.SessionUpdate, Err: err}
	}

	switch hdr.SessionUpdate {
	case KindAgentMessageChunk, KindAgentThoughtChunk, KindUserMessageChunk:
		var p struct {
			Content ContentBlock `json:"content"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return unknown(err)
		}
		switch hdr.SessionUpdate {
		case KindAgentMessageChunk:
			return AgentMessageChunk{Base: base, Content: p.Content}
		case KindAgentThoughtChunk:
			return AgentThoughtChunk{Base: base, Content: p.Content}
		default:
			return UserMessageChunk{Base: base, Content: p.Content}
		}

	case KindToolCall:
		var p ToolCallInfo
		if err := json.Unmarshal(raw, &p); err != nil {
			return unknown(err)
		}
		return ToolCall{Base: base, ToolCallInfo: p}

	case KindToolCallUpdate:
		var p ToolCallInfo
		if err := json.Unmarshal(raw, &p); err != nil {
			return unknown(err)
		}
		return ToolCallUpdate{Base: base, ToolCallInfo: p}

	case KindPlan:
		var p struct {
			Entries []PlanEntry `json:"entries"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return unknown(err)
		}
		return Plan{Base: base, Entries: p.Entries}

	case KindUsageUpdate:
		var p UsageUpdate
		if err := json.Unmarshal(raw, &p); err != nil {
			return unknown(err)
		}
		p.Base = base
		return p

	case KindAvailableCommands:
		var p struct {
			AvailableCommands []Command `json:"availableCommands"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return unknown(err)
		}
		return AvailableCommands{Base: base, Commands: p.AvailableCommands}

	case KindCurrentModeUpdate:
		var p struct {
			CurrentModeID string `json:"currentModeId"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return unknown(err)
		}
		return CurrentModeUpdate{Base: base, ModeID: p.CurrentModeID}

	case KindConfigOptionUpdate:
		var p struct {
			ConfigOptions json.RawMessage `json:"configOptions"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return unknown(err)
		}
		return ConfigOptionUpdate{Base: base, Options: p.ConfigOptions}

	case KindSessionInfoUpdate:
		var p struct {
			Title string `json:"title"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return unknown(err)
		}
		return SessionInfoUpdate{Base: base, Title: p.Title}

	default:
		return unknown(nil)
	}
}

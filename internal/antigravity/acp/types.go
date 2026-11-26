package acp

// types.go — THE REQUEST/RESPONSE PAYLOADS, as recorded from agy 1.5.13
// (docs/adapters/antigravity-acp.md, and the fixtures under internal/antigravity/testdata/acp).
//
// Unknown fields are deliberately preserved where a caller might need them later (_meta,
// configOptions, modes, tool args/rawInput/rawOutput) — agy adds fields between releases and a
// client that decodes strictly stops working on the next one.

import (
	"encoding/json"
	"sort"
)

// ACP method names. Spelled once here so a typo is a compile error rather than a wire bug.
const (
	methodInitialize        = "initialize"
	methodAuthenticate      = "authenticate"
	methodSessionNew        = "session/new"
	methodSessionLoad       = "session/load"
	methodSessionResume     = "session/resume"
	methodSessionList       = "session/list"
	methodSessionSetModel   = "session/set_model"
	methodSessionSetMode    = "session/set_mode"
	methodSessionPrompt     = "session/prompt"
	methodSessionCancel     = "session/cancel"
	methodSessionClose      = "session/close"
	methodSessionUpdate     = "session/update"
	methodRequestPermission = "session/request_permission"

	protocolVersion   = 1
	defaultClientName = "orchicon"
)

// ClientInfo identifies this client in `initialize`.
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// FileSystemCapabilities advertise the fs/* methods a client can serve.
//
// DECISION: both are false. agy 1.5.13 never issues fs/* or terminal/* requests — it reads and
// writes the workspace itself (docs/adapters/antigravity-acp.md 2.3) — so claiming them would
// only invite a call this client does not implement.
type FileSystemCapabilities struct {
	ReadTextFile  bool `json:"readTextFile"`
	WriteTextFile bool `json:"writeTextFile"`
}

// ClientCapabilities is the capability block sent on `initialize`.
type ClientCapabilities struct {
	FS       FileSystemCapabilities `json:"fs"`
	Terminal bool                   `json:"terminal"`
}

// InitializeParams is the `initialize` request.
type InitializeParams struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientCapabilities ClientCapabilities `json:"clientCapabilities"`
	ClientInfo         ClientInfo         `json:"clientInfo"`
}

// PromptCapabilities says which non-text prompt content types the agent accepts.
type PromptCapabilities struct {
	Image           bool `json:"image"`
	Audio           bool `json:"audio"`
	EmbeddedContext bool `json:"embeddedContext"`
}

// AgentCapabilities is agy's self-description from `initialize`.
type AgentCapabilities struct {
	LoadSession         bool                       `json:"loadSession"`
	PromptCapabilities  PromptCapabilities         `json:"promptCapabilities"`
	SessionCapabilities map[string]json.RawMessage `json:"sessionCapabilities"`
	Auth                map[string]json.RawMessage `json:"auth"`
	Meta                json.RawMessage            `json:"meta"`

	// Raw is the whole agentCapabilities object, for fields this struct does not model.
	Raw json.RawMessage `json:"-"`
}

// SessionCapability reports whether agy advertised one session capability ("load", "resume",
// "close", "list", "additionalDirectories").
func (a AgentCapabilities) SessionCapability(name string) bool {
	_, ok := a.SessionCapabilities[name]
	return ok
}

// AgentInfo identifies the agent process in `initialize`'s result.
type AgentInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// AuthMethod is one way agy can authenticate. The id is what `authenticate` takes.
type AuthMethod struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// InitializeResult is the `initialize` response.
type InitializeResult struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities AgentCapabilities `json:"agentCapabilities"`
	AgentInfo         AgentInfo         `json:"agentInfo"`
	AuthMethods       []AuthMethod      `json:"authMethods"`
	Meta              json.RawMessage   `json:"meta"`

	Raw json.RawMessage `json:"-"`
}

// AuthenticateParams is the `authenticate` request.
type AuthenticateParams struct {
	MethodID string          `json:"methodId"`
	Meta     json.RawMessage `json:"_meta,omitempty"`
}

// MCPServer is one MCP server declaration for `session/new`.
//
// THE STDIO SHAPE IS THE RECORDED ONE: `{name, command, args}` (mcp_tool_call.jsonl sends exactly
// that and agy connects). Two fields exist for the shapes the recording did not exercise:
//
//   - Env is a JSON value on purpose, because ACP declares a stdio server's environment as a
//     BEGIN/END list of `{name,value}` entries rather than a map. We render that list; the recording
//     simply never sent one.
//   - Type/Headers describe a REMOTE server, which agy 1.5.13 was never recorded accepting. A caller
//     that renders one should say so (mcp.go logs it) rather than assume it works.
type MCPServer struct {
	Name    string          `json:"name"`
	Command string          `json:"command,omitempty"`
	Args    []string        `json:"args,omitempty"`
	Env     json.RawMessage `json:"env,omitempty"`
	URL     string          `json:"url,omitempty"`
	// Type selects the transport for a remote server ("http"). Omitted for stdio, so the recorded
	// shape is byte-identical.
	Type string `json:"type,omitempty"`
	// Headers are the remote server's extra request headers, as the same {name,value} list shape as
	// Env.
	Headers json.RawMessage `json:"headers,omitempty"`
}

// EnvEntry is one `{name,value}` pair of an ACP environment/header list.
type EnvEntry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// EnvList renders a map as the ACP `{name,value}` list, sorted by name so the wire form is stable
// (Go map order is not) and a diff of two launches is readable. An empty map returns nil so the key
// is omitted entirely.
func EnvList(m map[string]string) json.RawMessage {
	if len(m) == 0 {
		return nil
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	entries := make([]EnvEntry, 0, len(names))
	for _, k := range names {
		entries = append(entries, EnvEntry{Name: k, Value: m[k]})
	}
	b, err := json.Marshal(entries)
	if err != nil {
		return nil
	}
	return b
}

// systemPromptMeta is the `_meta.antigravity.systemPrompt.append` block. agy only accepts a
// STRING here (handshake_auth.jsonl sent a number once and agy ignored the hint), so this client
// emits the block only when it has a real string.
type systemPromptMeta struct {
	Antigravity struct {
		SystemPrompt struct {
			Append string `json:"append"`
		} `json:"systemPrompt"`
	} `json:"antigravity"`
}

// newSessionMeta builds the _meta block, or nil when there is nothing to append.
func newSessionMeta(appendPrompt string) *systemPromptMeta {
	if appendPrompt == "" {
		return nil
	}
	m := &systemPromptMeta{}
	m.Antigravity.SystemPrompt.Append = appendPrompt
	return m
}

// NewSessionParams is the `session/new` request.
type NewSessionParams struct {
	Cwd                   string
	MCPServers            []MCPServer
	AdditionalDirectories []string
	// SystemPromptAppend is injected as _meta.antigravity.systemPrompt.append.
	SystemPromptAppend string
}

// newSessionWire is the on-wire `session/new` params.
type newSessionWire struct {
	Cwd                   string            `json:"cwd"`
	MCPServers            []MCPServer       `json:"mcpServers"`
	AdditionalDirectories []string          `json:"additionalDirectories,omitempty"`
	Meta                  *systemPromptMeta `json:"_meta,omitempty"`
}

// LoadParams is the caller-facing shape for `session/load` and `session/resume`.
type LoadParams struct {
	Cwd                   string
	MCPServers            []MCPServer
	AdditionalDirectories []string
	SystemPromptAppend    string
}

// loadWire is the on-wire `session/load` / `session/resume` params.
type loadWire struct {
	SessionID             string            `json:"sessionId"`
	Cwd                   string            `json:"cwd,omitempty"`
	MCPServers            []MCPServer       `json:"mcpServers"`
	AdditionalDirectories []string          `json:"additionalDirectories,omitempty"`
	Meta                  *systemPromptMeta `json:"_meta,omitempty"`
}

// Model is one entry of `models.availableModels`.
type Model struct {
	ModelID string `json:"modelId"`
	Name    string `json:"name"`
}

// Models is the model catalogue returned by session/new|load|resume.
type Models struct {
	AvailableModels []Model `json:"availableModels"`
	CurrentModelID  string  `json:"currentModelId"`
}

// SessionState is the reusable part of a session/new|load|resume result.
type SessionState struct {
	// SessionID is the id agy assigned (equal to the id passed to load/resume).
	SessionID string
	Models    []Model
	// CurrentModelID is the model agy reports as current at creation time. NOTE that agy may
	// report the FAMILY ("gemini-4-neon") here while availableModels lists thinking-level
	// variants ("gemini-4-neon-high"); see simple_turn.jsonl.
	CurrentModelID string
	Modes          json.RawMessage
	ConfigOptions  json.RawMessage
	Meta           json.RawMessage
	// Raw is the whole result object.
	Raw json.RawMessage
}

// sessionResult is the wire shape of session/new|load|resume. `sessionId` is absent from some
// replays (the id already came from the request), so callers fall back to the requested id.
type sessionResult struct {
	SessionID     string          `json:"sessionId"`
	Models        Models          `json:"models"`
	Modes         json.RawMessage `json:"modes"`
	ConfigOptions json.RawMessage `json:"configOptions"`
	Meta          json.RawMessage `json:"_meta"`

	// Raw is the whole result object, kept because agy adds fields between releases.
	Raw json.RawMessage `json:"-"`
}

// UnmarshalJSON keeps the raw result alongside the decoded fields.
func (r *sessionResult) UnmarshalJSON(b []byte) error {
	type alias sessionResult
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*r = sessionResult(a)
	r.Raw = append(json.RawMessage(nil), b...)
	return nil
}

// SessionInfo is one entry of `session/list`.
type SessionInfo struct {
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Title     string `json:"title"`
	UpdatedAt string `json:"updatedAt"`
}

type listResult struct {
	Sessions []SessionInfo `json:"sessions"`
}

// Usage is the per-turn token accounting agy attaches to a `session/prompt` result.
type Usage struct {
	InputTokens       int64 `json:"inputTokens"`
	OutputTokens      int64 `json:"outputTokens"`
	CachedReadTokens  int64 `json:"cachedReadTokens"`
	CachedWriteTokens int64 `json:"cachedWriteTokens"`
	ThoughtTokens     int64 `json:"thoughtTokens"`
	TotalTokens       int64 `json:"totalTokens"`
}

// Stop reasons agy returns from `session/prompt`.
const (
	StopReasonEndTurn   = "end_turn"
	StopReasonCancelled = "cancelled"
)

// PromptResult is the `session/prompt` response. It is the ONLY turn terminator: agy streams
// updates as notifications and then answers the prompt request once.
type PromptResult struct {
	StopReason string          `json:"stopReason"`
	Usage      Usage           `json:"usage"`
	Meta       json.RawMessage `json:"_meta"`
}

// setModelParams / setModeParams / sessionIDParams are the small session requests.
type setModelParams struct {
	SessionID string `json:"sessionId"`
	ModelID   string `json:"modelId"`
}

type setModeParams struct {
	SessionID string `json:"sessionId"`
	ModeID    string `json:"modeId"`
}

type sessionIDParams struct {
	SessionID string `json:"sessionId"`
}

type promptParams struct {
	SessionID string         `json:"sessionId"`
	Prompt    []ContentBlock `json:"prompt"`
}

// ContentBlock is one entry of a prompt. agy accepts plain text and, because it advertises
// promptCapabilities.embeddedContext, embedded context (ACP's "resource" content).
type ContentBlock struct {
	Type     string            `json:"type"`
	Text     string            `json:"text,omitempty"`
	Resource *EmbeddedResource `json:"resource,omitempty"`
}

// EmbeddedResource is ACP's embeddedContext resource: a URI plus its text.
type EmbeddedResource struct {
	URI      string `json:"uri"`
	Text     string `json:"text,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

// TextBlock is a plain text prompt block.
func TextBlock(s string) ContentBlock {
	return ContentBlock{Type: "text", Text: s}
}

// EmbeddedContextBlock wraps a file or snippet as ACP embedded context. The shape is standard
// ACP ("resource" with uri/text/mimeType); agy advertises embeddedContext support on initialize
// but the recording never exercised it, so this is the protocol shape rather than a recorded
// one.
func EmbeddedContextBlock(uri, text, mime string) ContentBlock {
	return ContentBlock{Type: "resource", Resource: &EmbeddedResource{URI: uri, Text: text, MimeType: mime}}
}

// PermissionOption is one answer agy offers on a `session/request_permission`. OptionID is what
// goes back; Kind ("allow_once"/"allow_always"/"reject_once"/"reject_always") is what the
// fail-closed decision reasons about.
type PermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

// Option kinds seen from agy (docs/adapters/antigravity-acp.md 3.3).
const (
	OptionKindAllowOnce    = "allow_once"
	OptionKindAllowAlways  = "allow_always"
	OptionKindRejectOnce   = "reject_once"
	OptionKindRejectAlways = "reject_always"
)

// permissionParams is the `session/request_permission` request body.
type permissionParams struct {
	SessionID string             `json:"sessionId"`
	ToolCall  ToolCallInfo       `json:"toolCall"`
	Options   []PermissionOption `json:"options"`
}

// permissionOutcome is the answer to a permission request: either a selected optionId or a bare
// `cancelled`. Both shapes are recorded (tool_permission_reject.jsonl vs the trailing answer in
// cancel_midturn.jsonl).
type permissionOutcome struct {
	Outcome struct {
		Outcome  string `json:"outcome"`
		OptionID string `json:"optionId,omitempty"`
	} `json:"outcome"`
}

// SelectedPermissionOutcome selects an optionId.
func SelectedPermissionOutcome(optionID string) permissionOutcome {
	var o permissionOutcome
	o.Outcome.Outcome = "selected"
	o.Outcome.OptionID = optionID
	return o
}

// CancelledPermissionOutcome answers a permission request with no selection.
func CancelledPermissionOutcome() permissionOutcome {
	var o permissionOutcome
	o.Outcome.Outcome = "cancelled"
	return o
}

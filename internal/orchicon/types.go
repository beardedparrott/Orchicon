package orchicon

import "context"

// --- Normalized event model (D2) ------------------------------------------
//
// Providers stream normalized events: every wire client converts at the
// edge so adapter/compaction consumers see one shape.

// Event is one normalized stream event. Concrete types below.
type Event interface{ isEvent() }

// TextDelta is a chunk of assistant text content.
type TextDelta struct{ Text string }

// ReasoningDelta is a chunk of assistant reasoning/thinking content.
// Reasoning is NEVER replayed back into history by any client.
type ReasoningDelta struct{ Text string }

// ToolCallStart marks the beginning of tool call `Index`.
type ToolCallStart struct {
	Index      int
	ToolCallID string
	Name       string
}

// ToolCallDelta is a fragment of the tool call's JSON arguments.
type ToolCallDelta struct {
	Index         int
	ArgsJSONDelta string
}

// ToolCallEnd marks the end of tool call `Index` (arguments complete).
type ToolCallEnd struct{ Index int }

// ToolCall is a complete tool invocation with ready JSON arguments.
type ToolCall struct {
	Index      int
	ToolCallID string
	Name       string
	ArgsJSON   string
}

// StreamError carries a mid-stream failure to the consumer (the stream
// then ends with an error from Next).
type StreamError struct{ Err error }

// Finish is the terminal event. StopReason is normalized.
type Finish struct {
	StopReason StopReason
	Usage      Usage
}

func (TextDelta) isEvent()      {}
func (ReasoningDelta) isEvent() {}
func (ToolCallStart) isEvent()  {}
func (ToolCallDelta) isEvent()  {}
func (ToolCallEnd) isEvent()    {}
func (ToolCall) isEvent()       {}
func (StreamError) isEvent()    {}
func (Finish) isEvent()         {}

// StopReason normalizes provider terminations.
type StopReason string

const (
	StopStop          StopReason = "stop"
	StopLength        StopReason = "length"
	StopToolUse       StopReason = "tool_use"
	StopContentFilter StopReason = "content_filter"
	StopError         StopReason = "error"
	StopOther         StopReason = "other"
)

// Usage carries token accounting. ReasoningTokens is a SUB-BUCKET of
// OutputTokens — never summed into totals (matches the usage-records cache
// migration 20260828). InputTokens is the FRESH (uncached) input bucket on
// EVERY wire: Anthropic input_tokens already excludes cache reads, and the
// OpenAI-compat client subtracts prompt_tokens_details.cached_tokens from
// prompt_tokens at the edge (prompt_tokens is cache-INCLUSIVE — see
// oaNoCache / legacyNoCache) so cache tokens are never double-counted in the
// window-pressure basis InputTokens+CacheReadTokens or in CostFor pricing.
// CacheRead/CacheWrite are Anthropic cache_read_/cache_creation_input_tokens
// and OpenAI prompt_tokens_details.cached_tokens (OpenAI has no cache-write
// → 0). CostUSD is the LIVE provider-priced cost of this turn (resolved by
// the wire client from the model's catalog/probe pricing); 0 when the model
// has no pricing — the budget cost gate then never fires on it, never a
// synthesized estimate.
type Usage struct {
	InputTokens      int64
	OutputTokens     int64
	ReasoningTokens  int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	CostUSD          float64
}

// TotalTokens is input + output (cache tokens are input sub-classes, and
// reasoning is an output sub-bucket — neither is added).
func (u Usage) TotalTokens() int64 { return u.InputTokens + u.OutputTokens }

// --- Request shape ---------------------------------------------------------

// CacheControlTTL is the breakpoint policy for cache_control placement.
type CacheControlTTL string

const (
	CacheControlNone           CacheControlTTL = "none"
	CacheControlSystem         CacheControlTTL = "system"
	CacheControlSystemAndTools CacheControlTTL = "system+tools"
)

// SystemBlock is one system-prompt block; Cache sets an Anthropic
// cache_control ephemeral breakpoint on this block.
type SystemBlock struct {
	Text  string
	Cache bool
}

// Role is a message role.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool" // tool result (OpenAI "tool" / Anthropic tool_result block)
)

// Content is one typed content element of a message.
type Content struct {
	// Exactly one of the pointer fields is set.
	Text *string

	// Image is a data URL (data:image/png;base64,...) — ImageInput-capable
	// providers only.
	Image *string

	// ToolUse is set on assistant messages requesting a tool call.
	ToolUse *ContentToolUse

	// ToolResult is set on tool-role messages returning a result.
	ToolResult *ContentToolResult
}

// ContentToolUse is an assistant-requested tool invocation in history.
type ContentToolUse struct {
	ToolCallID string
	Name       string
	ArgsJSON   string
}

// ContentToolResult is a tool result in history.
type ContentToolResult struct {
	ToolCallID string
	// Content is the result text.
	Content string
	// IsError marks a failed tool execution.
	IsError bool
}

// Message is one conversation message. History marshaling strips assistant
// reasoning/thinking content — reasoning is never replayed (D2).
type Message struct {
	Role    Role
	Content []Content
}

// ToolDef declares a callable tool with a JSON-schema parameters spec.
type ToolDef struct {
	Name        string
	Description string
	ParamsJSON  string // JSON schema for the parameters object
}

// TurnRequest is one provider turn request.
type TurnRequest struct {
	// Model is the bare model id (the registry owns the provider/ ref form).
	Model string

	System    []SystemBlock
	Messages  []Message
	Tools     []ToolDef
	MaxTokens int64

	// Temperature, nil = provider default.
	Temperature *float64

	// ReasoningEffort is "" | low | medium | high; sent ONLY for models
	// with known effort support (catalog/picker responsibility to set).
	ReasoningEffort string

	// OllamaNumCtx rides native /api/chat options.num_ctx (Ollama only;
	// OpenAI-compat does not accept num_ctx). 0 = server default.
	OllamaNumCtx int64

	// CacheControl is the Anthropic breakpoint policy (default: system+tools
	// for anthropic-format routes; ignored elsewhere).
	CacheControl CacheControlTTL
}

// Capabilities reports what a provider instance supports.
type Capabilities struct {
	Streaming        bool
	Tools            bool
	ReasoningEfforts bool // accepts reasoning_effort / thinking budgets
	ImageInput       bool
	CacheBreakpoints bool // Anthropic-style cache_control
	NativeMetadata   bool // Ollama native /api/show true metadata
}

// TurnStream is a normalized turn event stream.
type TurnStream interface {
	// Next returns the next event; ok=false means the stream ended. After
	// the Finish event the stream returns ok=false on the following call.
	Next(ctx context.Context) (Event, bool, error)
	// Close releases the underlying response body; safe to call twice.
	Close() error
}

// Provider is the provider-agnostic surface every backend implements:
// stream a turn, list models, report capabilities.
type Provider interface {
	// StreamTurn streams one turn. The returned error covers pre-stream
	// failures (auth, connect, non-2xx after retries); mid-stream failures
	// surface as a StreamError event followed by Next returning the error.
	StreamTurn(ctx context.Context, req TurnRequest) (TurnStream, error)
	// ListModels resolves through the sourcing service but is directly
	// callable on the provider.
	ListModels(ctx context.Context) ([]ModelInfo, error)
	// Capabilities reports the provider's feature surface.
	Capabilities() Capabilities
}

// --- Catalog-facing model shape (shared with sourcing; defined here to
// avoid an import from catalog back into the parent) -----------------------

// Pricing is per-million-token pricing for one token class group.
type Pricing struct {
	InputPerM      float64 `json:"input_per_m"`
	OutputPerM     float64 `json:"output_per_m"`
	CacheReadPerM  float64 `json:"cache_read_per_m,omitempty"`
	CacheWritePerM float64 `json:"cache_write_per_m,omitempty"`
	Currency       string  `json:"currency"`
	Source         string  `json:"source"`
}

// ModelInfo is the unified model descriptor the catalog, the probe and
// manual entries all normalize into.
type ModelInfo struct {
	// ID is the bare model id (no provider/ prefix).
	ID string `json:"id"`

	// Context is the context window in tokens; 0 = unknown (picker WARNs,
	// compaction must not guess — see D8).
	Context int64 `json:"context,omitempty"`

	// MaxOutput is the max output tokens; 0 = unknown.
	MaxOutput int64 `json:"max_output,omitempty"`

	// Tools reports tool-calling support (nil = unknown).
	Tools *bool `json:"tools,omitempty"`

	// ReasoningEfforts lists supported effort levels (empty = none known).
	ReasoningEfforts []string `json:"reasoning_efforts,omitempty"`

	// Pricing is nil when unknown — displays zero cost with an explicit
	// "billing applies" disclaimer (never implies free).
	Pricing *Pricing `json:"pricing,omitempty"`

	// Visible is the per-provider visibility toggle default (catalog/entry).
	Visible bool `json:"visible"`

	// Aliases are alternate ids resolving to this model.
	Aliases []string `json:"aliases,omitempty"`

	// Provenance marks where this entry came from: "catalog", "probe",
	// "manual".
	Provenance string `json:"provenance,omitempty"`
}

// SupportsTools resolves the tri-state tools field.
func (m ModelInfo) SupportsTools() bool { return m.Tools != nil && *m.Tools }

// SupportsReasoningEffort reports known effort-level support.
func (m ModelInfo) SupportsReasoningEffort() bool { return len(m.ReasoningEfforts) > 0 }

// HasPricing reports whether pricing is known (false → "billing applies"
// disclaimer in picker metadata).
func (m ModelInfo) HasPricing() bool { return m.Pricing != nil }

// CostFor computes the cost of one usage record in the pricing currency.
// Used by pickers and cost tests; 0 when pricing is unknown.
func (m ModelInfo) CostFor(u Usage) float64 {
	if m.Pricing == nil {
		return 0
	}
	p := m.Pricing
	return float64(u.InputTokens)/1e6*p.InputPerM +
		float64(u.OutputTokens)/1e6*p.OutputPerM +
		float64(u.CacheReadTokens)/1e6*p.CacheReadPerM +
		float64(u.CacheWriteTokens)/1e6*p.CacheWritePerM
}

package aigateway

// AnthropicUsage is the token-bucket shape Anthropic / Claude Code reports
// in its usage object. Field names use the provider's wire vocabulary so
// the mapping in UsageFromAnthropic is unambiguous (docs
// canonical-usage-sample-contract §3).
//
//	Anthropic wire            → canonical
//	input_tokens              → PromptTokens
//	cache_read_input_tokens   → CacheReadTokens
//	cache_creation_input_tokens → CacheWriteTokens   (creation = cache write)
//	output_tokens             → CompletionTokens
//	reasoning_tokens          → ReasoningTokens (optional, sub-bucket of output)
type AnthropicUsage struct {
	InputTokens              int64
	CacheReadInputTokens     int64
	CacheCreationInputTokens int64
	OutputTokens             int64
	ReasoningTokens          int64
}

// UsageFromAnthropic maps an Anthropic/Claude Code usage object onto the
// canonical provider-agnostic UsageInput. It is pure: it only fills the
// token buckets on the supplied base input, leaving identity/context
// fields (TenantID, Provider, Model, CorrelationID, ...) untouched.
//
// TotalTokens is deliberately left unset here — it is owned by
// UsageRecorder.Record, which derives it as the four-bucket sum (reasoning
// not additive). This keeps the "one owner, one change" invariant: the
// transformer normalizes shape, the gateway applies the accounting rule.
func UsageFromAnthropic(usage AnthropicUsage, base UsageInput) UsageInput {
	base.PromptTokens = usage.InputTokens
	base.CacheReadTokens = usage.CacheReadInputTokens
	base.CacheWriteTokens = usage.CacheCreationInputTokens
	base.CompletionTokens = usage.OutputTokens
	base.ReasoningTokens = usage.ReasoningTokens
	return base
}

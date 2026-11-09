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

// UsageFromAnthropic maps an Anthropic/Claude Code usage object onto the canonical
// provider-agnostic UsageInput.
func UsageFromAnthropic(usage AnthropicUsage, base UsageInput) UsageInput {
	base.PromptTokens = usage.InputTokens
	base.CacheReadTokens = usage.CacheReadInputTokens
	base.CacheWriteTokens = usage.CacheCreationInputTokens
	base.CompletionTokens = usage.OutputTokens
	base.ReasoningTokens = usage.ReasoningTokens
	return base
}

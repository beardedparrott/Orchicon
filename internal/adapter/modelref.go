package adapter

import "strings"

// DefaultAdapterKind is the adapter kind assumed when a model_ref has no
// explicit adapter segment (a 1- or 2-segment ref like
// "deepseek-v4-flash" or "opencode/deepseek-v4-flash"). Today every
// worker model_ref in the tree is 1- or 2-segment, and "opencode" is the
// only registered adapter — the backward-compatible default.
const DefaultAdapterKind = "opencode"

// ModelRef is the parsed form of a worker model_ref. The model_ref
// grammar (model-ref namespace task, ADR-0003) is the SINGLE source of
// truth for the adapter kind:
//
//	<adapter>/<provider>/<model>   3-segment (new — explicit adapter kind)
//	<provider>/<model>             2-segment (legacy — adapter defaults to
//	                               DefaultAdapterKind)
//	<model>                        1-segment (legacy test/dev refs — adapter
//	                               defaults to DefaultAdapterKind)
//
// Examples:
//
//	opencode/anthropic/claude-sonnet-4  → Adapter "opencode"
//	opencode/deepseek-v4-flash          → Adapter "opencode" (legacy)
//	claude/anthropic/claude-sonnet-4    → Adapter "claude"
//	anthropic/claude-sonnet-4           → Adapter "opencode" (legacy)
//	deepseek-v4-flash                   → Adapter "opencode" (legacy)
type ModelRef struct {
	Adapter  string // the adapter kind (adapter segment, or the legacy default)
	Provider string // the model provider segment
	Model    string // the model id segment
}

// ParseModelRef parses a worker model_ref into its segments. The adapter
// kind is the first segment of a 3-segment ref; a 1- or 2-segment
// (legacy) ref defaults the adapter to DefaultAdapterKind. The remaining
// segments (provider, model) are parsed for completeness — the adapter
// kind is the only segment the scheduler consumes.
//
// An empty ref (or one that is only whitespace/separators) yields an
// empty ModelRef with Adapter "" — the caller resolves that to an
// actionable "no adapter kind could be parsed" error rather than
// guessing.
func ParseModelRef(ref string) ModelRef {
	parts := splitSegments(ref)
	switch len(parts) {
	case 3:
		return ModelRef{Adapter: parts[0], Provider: parts[1], Model: parts[2]}
	case 2:
		return ModelRef{Adapter: DefaultAdapterKind, Provider: parts[0], Model: parts[1]}
	case 1:
		return ModelRef{Adapter: DefaultAdapterKind, Model: parts[0]}
	default:
		return ModelRef{}
	}
}

// splitSegments splits a ref on "/" and trims whitespace, dropping empty
// segments (so "opencode//deepseek" normalizes to 2 segments).
func splitSegments(ref string) []string {
	trimmed := strings.TrimSpace(ref)
	if trimmed == "" {
		return nil
	}
	raw := strings.Split(trimmed, "/")
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

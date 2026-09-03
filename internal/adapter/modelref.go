package adapter

import (
	"fmt"
	"strings"
)

// DefaultAdapterKind is the adapter kind assumed when a model_ref has no
// explicit adapter segment (a 1- or 2-segment legacy ref). "opencode" is
// the only adapter wired into the dispatcher today; legacy refs like
// "opencode/deepseek-v4-flash-free" and "anthropic/claude-sonnet-4" keep
// dispatching exactly as they did before the adapter namespace landed.
const DefaultAdapterKind = "opencode"

// ModelRef is the parsed form of a worker model_ref under the pinned
// left-greedy grammar (ADR-0003):
//
//	<adapter>/<provider>/<model>   3+ segments (explicit adapter kind)
//	<provider>/<model>             2 segments (legacy — adapter inferred)
//	<model>                        1 segment (legacy — adapter inferred)
//
// The model field is the remainder after the first two segments, VERBATIM
// (internal slashes preserved — e.g. the Command Code ref
// "orchicon/commandcode/deepseek/deepseek-v4-flash" parses to
// Model "deepseek/deepseek-v4-flash"). The parser NEVER splits the model
// field at an internal slash, and model-id validation must not forbid "/".
type ModelRef struct {
	Adapter  string // segment 1 (3+ seg) or the inferred legacy default
	Provider string // segment 2 (3+ seg) or segment 1 (legacy 2-seg)
	Model    string // verbatim remainder (internal slashes preserved)
}

// ParseModelRef parses a model ref under the pinned grammar (left-greedy,
// exactly three semantic fields).
//
//   - "adapter/provider/model" and deeper ("adapter/provider/a/b/c") parse
//     left-greedy with the model = verbatim remainder.
//   - A legacy 2-segment "provider/model" ref infers the adapter from the
//     default adapter kind when seg1 is a KNOWN provider of that adapter
//     (built-in profile OR tenant-created custom provider via reg). A
//     2-segment ref whose seg1 is a known adapter kind is rejected as
//     malformed (it should be written adapter/provider/model); an unknown
//     seg1 is rejected with a Settings → Adapters pointer.
//   - A 1-segment ref is a legacy bare model id; adapter defaults.
//
// reg may be nil (e.g. deep in the adapter layer where no tenant registry
// is available): validation of known providers/adapters is skipped and the
// ref is parsed structurally. All errors are actionable (ADR-0003 D4).
func ParseModelRef(ref string, reg ProviderRegistry) (ModelRef, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ModelRef{}, fmt.Errorf("model ref must not be empty (expected adapter/provider/model or the legacy provider/model)")
	}
	i := strings.IndexByte(ref, '/')
	hasSlash := i >= 0
	seg1, rest := splitFirst(ref)
	seg1 = strings.TrimSpace(seg1)
	if seg1 == "" {
		return ModelRef{}, fmt.Errorf("model ref %q must be adapter/provider/model (or the legacy provider/model)", ref)
	}
	if !hasSlash {
		// 1-segment legacy: a bare model id, adapter defaults.
		return ModelRef{Adapter: DefaultAdapterKind, Model: seg1}, nil
	}
	seg2, modelRest := splitFirst(rest)
	seg2 = strings.TrimSpace(seg2)
	if seg2 == "" {
		return ModelRef{}, fmt.Errorf("model ref %q must be adapter/provider/model (or the legacy provider/model)", ref)
	}
	if modelRest == "" {
		// 2-segment legacy: provider/model with the adapter inferred.
		if reg != nil {
			if reg.IsKnownProvider(DefaultAdapterKind, seg1) {
				return ModelRef{Adapter: DefaultAdapterKind, Provider: seg1, Model: seg2}, nil
			}
			if reg.IsKnownAdapter(seg1) {
				return ModelRef{}, fmt.Errorf("model ref %q: %q is an adapter kind, not a provider — use adapter/provider/model (e.g. %s/provider/model)", ref, seg1, seg1)
			}
			return ModelRef{}, fmt.Errorf("unknown provider %q in model ref %q — create it in Settings → Adapters (custom provider), or use adapter/provider/model", seg1, ref)
		}
		return ModelRef{Adapter: DefaultAdapterKind, Provider: seg1, Model: seg2}, nil
	}
	// 3+ segments: left-greedy. Segment 1 = adapter, segment 2 = provider,
	// the remainder = model VERBATIM (internal slashes preserved — the
	// pinned fix for slashed model ids like Command Code's).
	if reg != nil && !reg.IsKnownAdapter(seg1) {
		return ModelRef{}, fmt.Errorf("unknown adapter %q in model ref %q — register an adapter of that kind, or use adapter/provider/model with a known adapter (e.g. %s/provider/model)", seg1, ref, DefaultAdapterKind)
	}
	return ModelRef{Adapter: seg1, Provider: seg2, Model: modelRest}, nil
}

// AdapterKind extracts the adapter kind from a model ref WITHOUT
// validation — the routing view used by the dispatcher/reconciler (an
// unknown kind surfaces its actionable "register an adapter" error at
// Resolve). Left-greedy: segment 1 of a 3+ segment ref; DefaultAdapterKind
// for a 1- or 2-segment legacy ref; "" for an empty/malformed ref.
func AdapterKind(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	seg1, rest := splitFirst(ref)
	seg1 = strings.TrimSpace(seg1)
	if seg1 == "" {
		return ""
	}
	i := strings.IndexByte(ref, '/')
	hasSlash := i >= 0
	if !hasSlash {
		// 1-segment legacy: a bare model id, adapter defaults.
		return DefaultAdapterKind
	}
	if strings.TrimSpace(rest) == "" {
		// Trailing slash — malformed; the caller resolves that.
		return ""
	}
	seg2, _ := splitFirst(rest)
	if strings.TrimSpace(seg2) == "" {
		return ""
	}
	// 3+ segments: the adapter kind is segment 1. A 2-segment ref (no
	// further slash in rest) is legacy: adapter defaults to opencode.
	if strings.IndexByte(rest, '/') < 0 {
		return DefaultAdapterKind
	}
	return seg1
}

// SplitForServe splits a model ref into the provider/model pair the serve
// API expects (opencode serve still receives {"providerID","modelID"};
// the adapter segment is consumed by the control plane and dropped before
// the serve call — ADR-0003 D1/D6). Left-greedy over the first two
// segments: a 3+ segment ref uses segment 2 as the provider and the
// verbatim remainder as the model; a legacy 2-segment ref uses segment 1
// as the provider and segment 2 as the model. ok=false when no
// provider/model can be derived (empty or 1-segment ref).
//
// The 3+-segment branch assumes CANONICAL refs (adapter/provider/model).
// Pre-namespace legacy refs with a slashed head (e.g. "commandcode/a/b")
// are NOT canonical: their head is a legacy provider id, not an adapter
// kind, and left-greedy would yield provider=seg2 — the WRONG serve pair.
// The canonicalize-pre-namespace-refs migration (20260917000000) rewrites
// such rows into canonical form, and worker validation's phantom-kind rule
// (internal/worker/validate.go) rejects new saves of that shape, so this
// path only ever sees canonical 3+-segment refs in practice. It is kept
// purely structural (no registry access) because serve/usage callers deep
// in the adapter layer have no tenant registry.
func SplitForServe(ref string) (provider, model string, ok bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", "", false
	}
	seg1, rest := splitFirst(ref)
	seg1 = strings.TrimSpace(seg1)
	if rest == "" || seg1 == "" {
		return "", "", false
	}
	seg2, modelRest := splitFirst(rest)
	seg2 = strings.TrimSpace(seg2)
	if seg2 == "" {
		return "", "", false
	}
	if modelRest == "" {
		// 2-segment legacy: provider/model.
		return seg1, seg2, true
	}
	// 3+ segments: provider = segment 2, model = verbatim remainder.
	return seg2, modelRest, true
}

// splitFirst splits ref at its first "/", returning the head and the
// remainder ("" when there is no separator).
func splitFirst(ref string) (head, rest string) {
	i := strings.IndexByte(ref, '/')
	if i < 0 {
		return ref, ""
	}
	return ref[:i], ref[i+1:]
}

// NormalizeRef rewrites a PRE-NAMESPACE legacy model ref into canonical
// adapter/provider/model form. Legacy refs are free-form provider/<model>
// entries written before the adapter namespace existed (ADR-0003): their
// head can be a plain provider id (opencode-go), a slashed model path
// (commandcode/deepseek/deepseek-v4-flash), or a bare model id. The new
// left-greedy grammar misreads such refs — head becomes an "adapter kind",
// which poisons adapter-change validation (a phantom adapter selection
// nobody made), dispatch row-kind resolution, and SplitForServe's serve
// pair. NormalizeRef converts them to the canonical shape:
//
//	opencode-go/deepseek-v4-flash   → opencode/opencode-go/deepseek-v4-flash
//	commandcode/a/b/c               → opencode/commandcode/a/b/c
//	bare-model                      → opencode/bare-model  (bare ids need no rewrite)
//	opencode/anthropic/m            → unchanged (already canonical)
//
// knownAdapters decides the legacy-vs-canonical cut: a head that IS a
// registered adapter kind means the ref already carries an adapter segment
// and is returned unchanged; anything else is treated as legacy and
// normalized under DefaultAdapterKind. Pass nil to normalize structurally
// (every 1- and 2-segment ref, plus ANY 3+ segment ref whose head is not
// a known kind — used by the data migration where the dispatcher's live
// kinds are unavailable). Refs are otherwise returned VERBATIM (no
// re-splitting of model segments, no trimming beyond the stored value).
func NormalizeRef(ref string, knownKinds map[string]struct{}) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	first, rest := splitFirst(ref)
	first = strings.TrimSpace(first)
	if first == "" {
		return ref // malformed (empty head); leave for parse-time errors
	}
	if knownKinds != nil {
		if _, ok := knownKinds[first]; ok {
			return ref // already carries a registered adapter segment
		}
	}
	// No further slash → 2-segment legacy (provider/model) or 1-segment
	// bare id. Both normalize by prefixing the default adapter kind.
	if rest == "" || !strings.Contains(rest, "/") {
		return DefaultAdapterKind + "/" + ref
	}
	// 3+ segments with a non-kind head: legacy slashed provider ref.
	return DefaultAdapterKind + "/" + ref
}

// NormalizeRefForMigration is NormalizeRef for the data migration: it
// reports (normalized, changed) — changed=true only when the normalized
// form differs from the input, so the migration skips canonical and
// empty/malformed rows untouched.
func NormalizeRefForMigration(ref string, knownKinds map[string]struct{}) (string, bool) {
	out := NormalizeRef(ref, knownKinds)
	if out == "" {
		return ref, false
	}
	return out, out != ref
}

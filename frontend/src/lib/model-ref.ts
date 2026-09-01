// model-ref.ts - TypeScript mirror of the Go model_ref grammar
// (internal/adapter/modelref.go, ADR-0003).
//
// The grammar is left-greedy over exactly three semantic fields:
//
//   <adapter>/<provider>/<model>   3+ segments (explicit adapter kind)
//   <provider>/<model>             2 segments (legacy - adapter inferred as opencode)
//   <model>                        1 segment (legacy - adapter inferred as opencode)
//
// The model field is the remainder after the first two segments, VERBATIM
// (internal slashes preserved - e.g. "orchicon/command-code/deepseek/deepseek-v4-flash"
// parses to Model "deepseek/deepseek-v4-flash"). The parser NEVER splits the
// model field at an internal slash.

/** The adapter kind assumed for legacy refs with no explicit adapter segment. */
export const DEFAULT_ADAPTER_KIND = "opencode";

/** The parsed form of a model_ref. */
export interface ParsedModelRef {
  adapter: string; // segment 1 (3+ seg) or the inferred legacy default
  provider: string; // segment 2 (3+ seg) or segment 1 (legacy 2-seg); "" for 1-seg
  model: string; // verbatim remainder (internal slashes preserved)
  /** Raw ref verbatim, so unknown/malformed refs keep rendering flagged. */
  raw: string;
}

/**
 * parseModelRef parses a model ref under the pinned left-greedy grammar.
 * Structural parse only - no catalog validation (the frontend has no
 * registry; unknown adapter/provider/model is the picker's flagged-review
 * state, D5). Returns null for empty/malformed refs (a raw ref with no
 * slash and no segments, or empty segments).
 */
export function parseModelRef(ref: string | undefined | null): ParsedModelRef | null {
  const trimmed = (ref ?? "").trim();
  if (!trimmed) return null;

  const [seg1, ...rest] = trimmed.split("/");
  if (!seg1 || seg1.trim() === "") return null;

  if (rest.length === 0) {
    // 1-segment legacy: a bare model id, adapter defaults.
    return { adapter: DEFAULT_ADAPTER_KIND, provider: "", model: seg1, raw: trimmed };
  }
  const seg2 = rest[0];
  if (!seg2 || seg2.trim() === "") return null;

  if (rest.length === 1) {
    // 2-segment legacy: provider/model, adapter inferred.
    return { adapter: DEFAULT_ADAPTER_KIND, provider: seg1, model: seg2, raw: trimmed };
  }

  // 3+ segments: left-greedy - model is the verbatim remainder.
  return {
    adapter: seg1,
    provider: seg2,
    model: rest.slice(1).join("/"),
    raw: trimmed,
  };
}

/**
 * formatModelRef builds the canonical 3-segment ref "adapter/provider/model".
 * The model segment is written verbatim (may contain internal slashes).
 * Empty provider/model collapse the ref back toward legacy forms only when
 * absent - callers pass a full selection; the function itself never
 * fabricates segments.
 */
export function formatModelRef(adapter: string, provider: string, model: string): string {
  const a = adapter.trim();
  const p = provider.trim();
  const m = model.trim();
  if (!a) return m;
  if (!p) return m ? `${a}/${m}` : a;
  return m ? `${a}/${p}/${m}` : `${a}/${p}`;
}

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
// (internal slashes preserved - e.g. "orchicon/commandcode/deepseek/deepseek-v4-flash"
// parses to Model "deepseek/deepseek-v4-flash"). The parser NEVER splits the
// model field at an internal slash.

/** The adapter kind assumed for legacy refs with no explicit adapter segment. */
export const DEFAULT_ADAPTER_KIND = "opencode";

/**
 * ORCHICON_ADAPTER_KIND is the decided product default for FRESH adapter
 * selections (ADR-0005 D5): the native adapter kind this feature delivers.
 * The picker seeds its adapter tier with this kind when the stored ref is
 * empty AND the kind is Dispatcher-registered; otherwise it degrades to
 * DEFAULT_ADAPTER_KIND (opencode) — never a default that cannot dispatch.
 * It is deliberately NOT the backward-compat inference: legacy refs
 * without an adapter segment keep inferring opencode.
 */
export const ORCHICON_ADAPTER_KIND = "orchicon";

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
 * CatalogModelLike is the minimal catalog-entry shape model-match needs
 * (structural — no protobuf type import). `modelRef` is the catalog's
 * LEGACY 2-segment "providerId/id" (internal/aigateway OpenCodeModel);
 * `providerId`+`id` carry the authoritative segments.
 */
export interface CatalogModelLike {
  id: string;
  providerId: string;
  modelRef?: string; // legacy 2-segment "providerId/id"
}

/**
 * catalogModelMatches reports whether a catalog entry corresponds to the
 * model in a (parsed) model ref, by SEGMENTS — not by raw-string equality.
 * A catalog entry's modelRef is the legacy 2-segment "providerId/id", so
 * a raw comparison against a 3-segment ref ("opencode/anthropic/claude-4")
 * never matches; the adapter segment is inherently not part of the
 * catalog's identity (models are scoped per provider+id). A parsed ref is
 * REQUIRED: unknown shapes (null) deliberately never match, so callers
 * keep flagging them for review (ADR-0004 D5).
 */
export function catalogModelMatches(
  parsed: ParsedModelRef,
  model: CatalogModelLike,
): boolean {
  if (parsed.provider !== "") {
    if (parsed.provider === model.providerId && parsed.model === model.id) return true;
    // Fallback for entries with empty segments: compare against the legacy
    // 2-segment modelRef verbatim.
    return model.modelRef === `${parsed.provider}/${parsed.model}`;
  }
  // Legacy 1-segment ref: a bare model id with no provider segment.
  return parsed.model === model.id || parsed.model === model.modelRef;
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

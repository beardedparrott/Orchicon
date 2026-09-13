// ask-metrics-format.ts — the PURE half of the Ask stat strip: the shape, the
// fold over usage records, and the formatters.
//
// Split out from ask-metrics.ts so it can be tested (and reasoned about)
// without importing the API clients or any React hook, and so the TUI's
// equivalent (internal/tui/models.go) has one obvious counterpart to stay in
// step with.
import type { UsageRecord } from "@/api/gen/orchicon/api/v1/ai_gateway_pb";

export interface AskMetrics {
  model: string;
  ctxUsed: number;
  ctxWindow: number;
  tokens: number;
  cacheRead: number;
  prompt: number;
  costUsd: number;
  /** false until the first usage read lands (the strip still shows the model). */
  have: boolean;
}

/**
 * summarizeAskMetrics folds the usage records into the strip's numbers.
 *
 * Context occupancy is the LATEST record's input side (prompt + cache reads +
 * cache writes): records arrive newest-first, and the SUM across turns is not
 * the context size — the newest turn's input is the best available proxy for
 * what is currently in the window.
 */
export function summarizeAskMetrics(
  records: UsageRecord[],
  model: string,
  ctxWindow: number,
): AskMetrics {
  const out: AskMetrics = {
    model,
    ctxUsed: 0,
    ctxWindow,
    tokens: 0,
    cacheRead: 0,
    prompt: 0,
    costUsd: 0,
    have: records.length > 0,
  };
  records.forEach((r, i) => {
    out.tokens += Number(r.totalTokens ?? 0);
    out.cacheRead += Number(r.cacheReadTokens ?? 0);
    out.prompt += Number(r.promptTokens ?? 0);
    out.costUsd += Number(r.costUsd ?? 0);
    if (i === 0) {
      out.ctxUsed =
        Number(r.promptTokens ?? 0) + Number(r.cacheReadTokens ?? 0) + Number(r.cacheWriteTokens ?? 0);
    }
  });
  return out;
}

/** fmtTokens mirrors the Go helper exactly (124K, 1.2M) so the two agree. */
export function fmtTokens(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${Math.trunc(n / 1_000)}K`;
  return String(n);
}

/**
 * cacheHitRatio is the share of INPUT tokens served from the prompt cache:
 * cache_read / (cache_read + uncached input) — the two halves the recorder
 * stores separately. null when there is no input, so the strip never prints a
 * meaningless "0%".
 */
export function cacheHitRatio(cacheRead: number, prompt: number): string | null {
  const denom = cacheRead + prompt;
  if (denom <= 0) return null;
  return `${Math.round((cacheRead / denom) * 100)}%`;
}

/** fmtCtx shows occupancy against the window, omitting an unknown denominator. */
export function fmtCtx(used: number, window: number): string {
  return window > 0 ? `${fmtTokens(used)}/${fmtTokens(window)}` : fmtTokens(used);
}

/** formatAskMetricsLine renders the strip; "" when there is nothing to say. */
export function formatAskMetricsLine(m: AskMetrics): string {
  if (!m.model && !m.have) return "";
  const segs: string[] = [];
  if (m.model) segs.push(m.model);
  if (!m.have) return segs.join(" · ");
  segs.push(`ctx ${fmtCtx(m.ctxUsed, m.ctxWindow)}`);
  segs.push(`${fmtTokens(m.tokens)} tok`);
  const ratio = cacheHitRatio(m.cacheRead, m.prompt);
  if (ratio) segs.push(`cache ${ratio} (${fmtTokens(m.cacheRead)})`);
  segs.push(`$${m.costUsd.toFixed(4)}`);
  return segs.join(" · ");
}

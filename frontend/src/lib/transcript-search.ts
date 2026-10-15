// transcript-search.ts — the pure filter behind the Ask conversation's
// in-transcript search.
//
// Client-side over already-loaded messages: no server search API, no regex, no
// persisted history (all out of scope). Case-insensitive SUBSTRING is the whole
// rule, which is why it lives here as small pure functions a test can drive
// directly rather than inline in the (untestable) route.

/** normalizeQuery trims and lowercases — the canonical form the comparisons use. */
export function normalizeQuery(query: string): string {
  return query.trim().toLowerCase();
}

/** matchesQuery reports whether text contains the query (case-insensitive). */
export function matchesQuery(text: string, query: string): boolean {
  const q = normalizeQuery(query);
  if (q === "") return false;
  return text.toLowerCase().includes(q);
}

/**
 * filterByQuery keeps the items whose text matches. An EMPTY query is the
 * "no filter" state and returns the items unchanged. keep, when given, forces an
 * item to be retained regardless of its text — used so a consent/ask card (which
 * is not a message) is never hidden behind a text filter.
 */
export function filterByQuery<T>(
  items: T[],
  query: string,
  textOf: (item: T) => string,
  keep?: (item: T) => boolean,
): T[] {
  const q = normalizeQuery(query);
  if (q === "") return items;
  return items.filter(
    (it) => (keep ? keep(it) : false) || textOf(it).toLowerCase().includes(q),
  );
}

/** countMatches counts the items whose text matches (0 for an empty query). */
export function countMatches<T>(
  items: T[],
  query: string,
  textOf: (item: T) => string,
): number {
  const q = normalizeQuery(query);
  if (q === "") return 0;
  return items.reduce((n, it) => n + (textOf(it).toLowerCase().includes(q) ? 1 : 0), 0);
}

/**
 * searchStatusLabel is the toolbar's status text: an explicit empty state when
 * nothing matches, otherwise the match count. Empty for an inactive query (the
 * toolbar shows no status until something is typed).
 */
export function searchStatusLabel(
  query: string,
  matchCount: number,
  total: number,
): string {
  if (normalizeQuery(query) === "") return "";
  if (matchCount === 0) return "No matches";
  return `${matchCount}/${total} matches`;
}

/**
 * matchRanges finds every occurrence of the query in text, as [start, end) offsets.
 *
 * WHY OFFSETS RATHER THAN A SPLIT. The caller has to render the text in pieces — some wrapped in a mark,
 * some not — and offsets are the only form that lets it keep the ORIGINAL casing. A split on a
 * lowercased copy would return lowercased fragments, so a match would be displayed in the wrong case.
 *
 * OVERLAPPING MATCHES ARE NOT PRODUCED: one occurrence cannot start inside another of the same needle,
 * so the scan resumes after each match. Empty and whitespace-only queries match nothing — the "no
 * filter" state must not mark every position in the transcript.
 */
export function matchRanges(text: string, query: string): Array<[number, number]> {
  const q = normalizeQuery(query);
  if (q === "" || text === "") return [];
  const hay = text.toLowerCase();
  const out: Array<[number, number]> = [];
  let from = 0;
  for (;;) {
    const at = hay.indexOf(q, from);
    if (at === -1) break;
    out.push([at, at + q.length]);
    from = at + q.length;
  }
  return out;
}

/** One piece of text with its verdict: tagged when it is a match the caller should mark. */
export interface MatchPiece {
  text: string;
  match: boolean;
}

/**
 * splitMatches cuts text into alternating plain/matched pieces for rendering, preserving the original
 * case and the surrounding text exactly. Text with no matches comes back as ONE untagged piece, so a
 * caller that always goes through this path renders unmarked text byte-for-byte as before.
 */
export function splitMatches(text: string, query: string): MatchPiece[] {
  const ranges = matchRanges(text, query);
  if (ranges.length === 0) return [{ text, match: false }];
  const out: MatchPiece[] = [];
  let at = 0;
  for (const [start, end] of ranges) {
    if (start > at) out.push({ text: text.slice(at, start), match: false });
    out.push({ text: text.slice(start, end), match: true });
    at = end;
  }
  if (at < text.length) out.push({ text: text.slice(at), match: false });
  return out;
}

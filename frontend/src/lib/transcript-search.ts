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

import { splitMatches } from "@/lib/transcript-search";

// SearchHighlight — marks every occurrence of the in-transcript search query inside a run of text.
//
// WHY IT EXISTS. Search already filtered the transcript to matching messages and reported a count, but it
// marked NOTHING, so on a long reply the operator could see that a match was somewhere in the message and
// not where. That is the whole difficulty with a large stream of text: a message-level filter answers
// "which message", and the operator needs "which line". This answers the second question.
//
// IT WORKS ON THE ROW, NOT ON THE RENDERED MARKDOWN. Marking inside rendered markdown would mean
// re-parsing every message (and would corrupt any match that spans an inline element, e.g. text on both
// sides of a `code` chip). Highlighting the plain text a bubble is given is exact, cheap, and cannot
// change how the markdown itself renders.

export interface SearchHighlightProps {
  text: string;
  /** The active query. Empty renders the text unchanged — which is the default state. */
  query: string;
}

/**
 * MARK SEMANTICS ARE DELIBERATE. `<mark>` is the platform's own element for "this run is relevant to a
 * search", so a screen reader announces it without help and the browser gives it a highlight background
 * for free. The colours are overridden to theme tokens rather than the UA default, because the default is
 * a fixed yellow that clashes with every theme in the picker.
 */
export function SearchHighlight({ text, query }: SearchHighlightProps) {
  const pieces = splitMatches(text, query);
  // THE UNMARKED CASE MUST BE EXACTLY THE OLD OUTPUT. One piece, no wrapper, no extra element: a bubble
  // rendered with no search active is byte-for-byte what it was before search existed.
  if (pieces.length === 1 && !pieces[0].match) {
    return <>{text}</>;
  }
  return (
    <>
      {pieces.map((p, i) =>
        p.match ? (
          <mark
            key={i}
            data-testid="transcript-search-hit"
            className="rounded-sm bg-amber-400/40 text-inherit dark:bg-amber-300/30"
          >
            {p.text}
          </mark>
        ) : (
          <span key={i}>{p.text}</span>
        ),
      )}
    </>
  );
}

export default SearchHighlight;

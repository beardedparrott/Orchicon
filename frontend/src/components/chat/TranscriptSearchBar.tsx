import type { RefObject } from "react";
import { ChevronDown, ChevronUp, Search, X } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";
import { searchStatusLabel } from "@/lib/transcript-search";

interface TranscriptSearchBarProps {
  /** The controlled query (parent-owned so the filter and the bar cannot disagree). */
  query: string;
  onQueryChange: (query: string) => void;
  /** How many messages match the current query. */
  matchCount: number;
  /** How many messages the conversation holds (the denominator). */
  total: number;
  onNext: () => void;
  onPrev: () => void;
  onClear: () => void;
  /**
   * Escape clears the query and hands focus back to the composer. Falls back to
   * onClear when absent (a caller that does not own a composer to focus).
   */
  onEscape?: () => void;
  /**
   * Ref to the field, so the route's document-level Cmd/Ctrl+F chord can focus it.
   */
  inputRef?: RefObject<HTMLInputElement>;
}

/**
 * transcriptSearchKey maps a keypress in the search field to its action. It is PURE
 * so the keyboard contract is testable without a DOM (this repo has no jsdom or
 * @testing-library — component tests render server-side), and it is the single
 * source of truth for which keys the field consumes.
 *
 * BOTH STEP KEYS ARE NO-OPS WHEN NOTHING MATCHES: that is the exact state the
 * prev/next buttons are disabled on, so the keyboard respects it rather than
 * stepping an empty match set.
 */
export function transcriptSearchKey(
  key: string,
  shiftKey: boolean,
  matchCount: number,
): "clear" | "next" | "prev" | null {
  if (key === "Escape") return "clear";
  if (key === "Enter") {
    if (matchCount === 0) return null;
    return shiftKey ? "prev" : "next";
  }
  return null;
}

/**
 * TranscriptSearchBar is the Ask conversation's search field: type to filter the
 * transcript, step matches with prev/next, and clear to restore it.
 *
 * IT IS CONTROLLED AND PRESENTATIONAL. The parent owns the query and does the
 * filtering (with the unit-tested helpers in @/lib/transcript-search); this only
 * renders the input, the `n/m matches` status (or an explicit "No matches") and the
 * stepper. The status and the stepper stay hidden until a query is typed, so an
 * untouched conversation has no search chrome.
 */
export function TranscriptSearchBar({
  query,
  onQueryChange,
  matchCount,
  total,
  onNext,
  onPrev,
  onClear,
  onEscape,
  inputRef,
}: TranscriptSearchBarProps) {
  const status = searchStatusLabel(query, matchCount, total);
  const active = query.trim() !== "";
  const hasMatches = matchCount > 0;

  return (
    <div
      data-testid="transcript-search"
      className="flex shrink-0 items-center gap-2 border-b px-4 py-2 sm:px-6"
    >
      <div className="relative min-w-0 flex-1">
        <Search
          aria-hidden="true"
          className="pointer-events-none absolute left-2 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground"
        />
        <Input
          aria-label="Search transcript"
          placeholder="Search this conversation…"
          value={query}
          onChange={(e) => onQueryChange(e.target.value)}
          ref={inputRef}
          onKeyDown={(e) => {
            const action = transcriptSearchKey(e.key, e.shiftKey, matchCount);
            if (!action) return;
            e.preventDefault();
            if (action === "clear") (onEscape ?? onClear)();
            else if (action === "next") onNext();
            else onPrev();
          }}
          className="h-8 pl-8"
        />
      </div>
      {active && (
        <span
          data-testid="transcript-search-status"
          aria-live="polite"
          className={cn(
            "shrink-0 text-xs tabular-nums",
            hasMatches ? "text-muted-foreground" : "text-amber-600 dark:text-amber-400",
          )}
        >
          {status}
        </span>
      )}
      {active && (
        <div className="flex shrink-0 items-center gap-1">
          <Button
            type="button"
            variant="ghost"
            size="icon"
            aria-label="Previous match"
            data-testid="transcript-search-prev"
            onClick={onPrev}
            disabled={!hasMatches}
            className="h-8 w-8"
          >
            <ChevronUp aria-hidden="true" className="h-4 w-4" />
          </Button>
          <Button
            type="button"
            variant="ghost"
            size="icon"
            aria-label="Next match"
            data-testid="transcript-search-next"
            onClick={onNext}
            disabled={!hasMatches}
            className="h-8 w-8"
          >
            <ChevronDown aria-hidden="true" className="h-4 w-4" />
          </Button>
          <Button
            type="button"
            variant="ghost"
            size="icon"
            aria-label="Clear search"
            data-testid="transcript-search-clear"
            onClick={onClear}
            className="h-8 w-8"
          >
            <X aria-hidden="true" className="h-4 w-4" />
          </Button>
        </div>
      )}
    </div>
  );
}

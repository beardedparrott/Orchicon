import type { Ref } from "react";

interface ActivityLineProps {
  /** The full line text — the rotating verb plus, in the healthy band, the counter summary. */
  text: string;
  /** The STABLE string the status region announces (see activityLineAnnouncement). */
  announcement: string;
  /** The active fallback-model annotation, or null. Preserved from the bubble this replaces. */
  fallbackModel?: string | null;
  /** Forwarded to the root node so the route can measure the pane width (useRailWidth). */
  containerRef?: Ref<HTMLDivElement>;
}

/**
 * The transcript's activity line: what the turn is DOING, for the WHOLE turn.
 *
 * THE BUBBLE SHELL AND THE DOT ARE THE EXISTING TOKENS, unchanged — text-muted-foreground, the
 * sky-500 pulse, the same rounded-2xl shell the thinking bubble already drew. The theme system is
 * real (lib/themes.ts, theme-store.ts): no hardcoded colour appears here, so dark and light both
 * work. motion-reduce:animate-none honours the profile's reduce preference for the DOT as well as
 * the rotation (ask-verbs.activityVerb already freezes the WORD).
 *
 * IT IS NOT FOCUSABLE. There is no tabIndex, no button, no anchor — a status line that entered the
 * tab order would put a keystroke between the operator and the composer.
 *
 * ONE VOICE TO A SCREEN READER. The visible line is aria-hidden (its verb rotates every 15s and its
 * age every 1s — announcing either would be a machine gun), and the region's accessible content is
 * the stable announcement, which changes only when a call lands.
 *
 * NO HOOKS INSIDE, and that is load-bearing: props in, markup out keeps it renderable by
 * react-dom/server's renderToStaticMarkup in this repo's plain node Vitest run (no jsdom), which is
 * what makes the aria assertions real rather than source-text greps.
 */
export function ActivityLine({
  text,
  announcement,
  fallbackModel,
  containerRef,
}: ActivityLineProps) {
  return (
    <div ref={containerRef} className="flex justify-start">
      <div
        role="status"
        aria-live="polite"
        data-testid="ask-activity-line"
        className="max-w-[88%] rounded-2xl rounded-tl-sm border border-sky-300/30 bg-sky-50/20 px-4 py-3 dark:border-sky-950/40 dark:bg-sky-950/10"
      >
        <div className="flex items-center gap-2">
          <span
            aria-hidden="true"
            className="shrink-0 inline-block h-1.5 w-1.5 rounded-full bg-sky-500 animate-pulse motion-reduce:animate-none"
          />
          <span
            aria-hidden="true"
            className="min-w-0 text-sm text-muted-foreground [overflow-wrap:anywhere]"
          >
            {text}
            {fallbackModel && (
              <span className="text-muted-foreground/70">
                {" "}
                ({fallbackModel} — free fallback, may be rate-limited)
              </span>
            )}
          </span>
          <span className="sr-only">{announcement}</span>
        </div>
      </div>
    </div>
  );
}

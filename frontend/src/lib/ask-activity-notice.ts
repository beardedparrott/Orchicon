// ask-activity-notice.ts — WHEN the activity line is drawn, WHAT it says, and WHAT IT DROPS when the
// pane is too narrow. The Go half is internal/tui/app.go: turnActivityNotice + fitNotice; this is the
// port, and the DECISION function (activityLineFor) lives here rather than inline in the route
// because the route cannot be rendered by the test setup — logic left inline there is logic nothing
// can assert (the same reason lib/conversationProjects owns the folder-scope rule).

import { activityVerb } from "@/lib/ask-verbs";
import {
  TOOL_SUMMARY_WINDOW_MS,
  summarizeToolCalls,
  toolCountPhrase,
  type ToolCallStamp,
} from "@/lib/ask-tool-summary";

export const NOTICE_SHOW_AGE_AFTER_MS = 1_000; // mirrors showAgeAfter: the line is visible immediately
export const NOTICE_WARN_AFTER_MS = 25_000; // mirrors warnAfter: past the 15s heartbeat + slack
export const NOTICE_RE_DIAL_AFTER_MS = 35_000; // mirrors reDialAfter: the watchdog re-dials at 40s

/** The silence age, from the server's own last-activity stamp (the conversations list already
 *  reports it and the page already polls it). `null` -> 0, i.e. "no activity recorded yet" — the
 *  moment between sending and the stream's first event — which renders the BARE line rather than an
 *  absurd "0s ago". Negative (a clock that stepped backwards) clamps to 0, never a negative age. */
export function silentFor(lastActivityMs: number | null, nowMs: number): number {
  if (lastActivityMs === null || !Number.isFinite(lastActivityMs)) return 0;
  return Math.max(0, nowMs - lastActivityMs);
}

/**
 * Trim ONE line to the pane's width by dropping text in priority order:
 *   STEP 1 the summary (the count phrase and its age) — everything after the verb on a healthy line;
 *   STEP 2 the escalation band, leaving the verb;
 *   STEP 3 the widest RUNE PREFIX of the verb itself.
 * `width <= 0` means UNBOUNDED (not yet measured), and a line that already fits is returned as it
 * stands: this only ever REMOVES text. Mirrors internal/tui/app.go fitNotice.
 */
export function fitActivityNotice(line: string, verb: string, width: number): string {
  // A zero or negative width means "unbounded", and a line that already fits is returned as it
  // stands: the function only ever REMOVES text.
  if (width <= 0 || [...line].length <= width) return line;
  // STEP 1 — the summary goes first. It is everything the line carries beyond the verb's own
  // segment. The escalation band is recognisable by its wording, so a line carrying one is not
  // touched here (the band is step 2, not step 1).
  if (line.startsWith(verb)) {
    const rest = line.slice(verb.length);
    if (!rest.includes("no output for") && [...verb].length <= width) return verb;
  }
  // STEP 2 — the band goes next, leaving the verb.
  if ([...verb].length <= width) return verb;
  // STEP 3 — a pane narrower than the verb itself. The last resort is the widest RUNE prefix that
  // fits: a row that wraps is worse than a row that is short, and a byte slice could split the
  // ellipsis into invalid UTF-8. It is never empty.
  const runes = [...verb];
  const w = width < 1 ? 1 : width;
  return runes.slice(0, w).join("");
}

/**
 * The line's TEXT for a turn in flight. Bands mirror turnActivityNotice exactly.
 *
 * THE SUMMARY APPEARS IN THE HEALTHY BAND ONLY. Past 25s it is not merely outranked, it is ABSENT: a
 * turn that made five calls and then died must escalate, not glow, and a tool tally beside the
 * watchdog's verdict would read as work still happening.
 *
 * THE SUMMARY'S OWN TRAILING "newest call Ns ago" IS THE AGE, so on the healthy arm it REPLACES
 * "last activity Ns ago" rather than joining it — one age, one phrase, one row.
 */
export function activityNoticeText(
  silentMs: number,
  effectiveServerTimeMs: number,
  summary: string,
  width: number,
): string {
  // ONE WORD, FROM ONE PURE FUNCTION. activityVerb holds the reduced-motion off-switch too, so a
  // rotation the operator turned off and a rotation with no server stamp yet land on the same word
  // (the list's first) — "the line is present and says something true" either way.
  const verb = `Orchicon is ${activityVerb(effectiveServerTimeMs)}…`;
  const secs = Math.round(silentMs / 1000);
  let want: string;
  if (silentMs >= NOTICE_RE_DIAL_AFTER_MS) {
    want = `${verb} · no output for ${secs}s — the stream will re-attach if it stays silent`;
  } else if (silentMs >= NOTICE_WARN_AFTER_MS) {
    want = `${verb} · no output for ${secs}s`;
  } else if (summary !== "") {
    want = `${verb} · ${summary}`;
  } else if (silentMs <= 0 || silentMs < NOTICE_SHOW_AGE_AFTER_MS) {
    // No counted work and no age yet — the moment between sending and the stream's first event. The
    // bare line, rather than an absurd "0s ago". It STILL goes through fitActivityNotice, because the
    // one-row budget is a property of the LINE and not of the bands: a pane narrower than the verb
    // would otherwise wrap, and a wrapped footer is a SECOND row.
    return fitActivityNotice(verb, verb, width);
  } else {
    want = `${verb} · last activity ${secs}s ago`;
  }
  return fitActivityNotice(want, verb, width);
}

/** Everything the line's decision needs. All of it is already computed in the route — none of these
 *  fields is a new fetch. */
export interface ActivityLineInput {
  /** The active conversation id; "" (no conversation) draws nothing. */
  convId: string;
  /** A turn is in flight FOR THIS CONVERSATION — EITHER HALF (the live slot OR the server's
   *  turn_in_flight). This is the whole regression fix: the old gate was "before the first token". */
  turnInFlight: boolean;
  /** The socket for an acked turn dropped; the server-side collector is still running. OUTRANKS the
   *  activity line (internal/tui/app.go transcriptStatusLine's first case) — a counter that outranks
   *  "the connection is gone" would claim a liveness the plane cannot deliver. */
  reconnecting: boolean;
  /** The server's last-activity stamp, epoch ms, or null. */
  lastActivityMs: number | null;
  /** The server clock extrapolated to this repaint (see ask-verbs.extrapolateServerTime). */
  effectiveServerTimeMs: number;
  /** This repaint's local instant — the same clock `lastActivityMs` is compared against. */
  nowMs: number;
  /** The page's counted tool calls (ask-tool-summary.toolCallsFromMessages over `messages`). */
  toolCalls: readonly ToolCallStamp[];
  /** The measured pane width in px, or 0 for "not yet measured" (unbounded). */
  widthPx: number;
  /** A consent card is pending for this conversation: the card must not compete with the counter. */
  cardPending: boolean;
}

/** What the LINE SAYS, or null for "no line". Returning null rather than "" is deliberate: an empty
 *  string is a line that renders nothing, and the caller would have to remember which one it is. */
export function activityLineFor(input: ActivityLineInput): string | null {
  if (!input.convId) return null;
  if (input.reconnecting) return null; // reconnecting › everything (AC4/AC5)
  if (!input.turnInFlight) return null; // clears when the turn ends AND on conversation change
  const silentMs = silentFor(input.lastActivityMs, input.nowMs);
  // A pending consent card is the thing that needs attention; the line stays present but QUIET —
  // the bare verb, no counter, so two live surfaces never compete (permission-denied state).
  const summary = input.cardPending
    ? ""
    : summarizeToolCalls(input.toolCalls, input.nowMs, TOOL_SUMMARY_WINDOW_MS);
  // The measured width is CSS pixels; the Go line's width is terminal cells. The counters are the
  // first thing dropped, so an approximate scale is safe — it can only ever drop the counter early,
  // never clip the verb.
  const widthCells = input.widthPx > 0 ? Math.max(1, Math.floor(input.widthPx / 8)) : 0;
  return activityNoticeText(silentMs, input.effectiveServerTimeMs, summary, widthCells);
}

/** The screen reader's string: the STABLE half only. The rotating verb and the per-second age are
 *  visual animation (the line is aria-hidden inside the status region), so this changes only when a
 *  new call lands — never on a rotation, which is what "without spamming a screen reader on every
 *  rotation" asks for. */
export function activityLineAnnouncement(input: ActivityLineInput): string {
  if (input.cardPending) return "Orchicon is working — a permission request is waiting";
  const phrase = toolCountPhrase(input.toolCalls, input.nowMs, TOOL_SUMMARY_WINDOW_MS);
  return phrase === "" ? "Orchicon is working" : `Orchicon is working · ${phrase}`;
}

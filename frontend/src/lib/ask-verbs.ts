// ask-verbs.ts — THE ACTIVITY VERB ROTATES, INDEXED ON THE SERVER'S CLOCK (the GUI half of
// internal/tui/chat/verbs.go; the two lists are pinned to ask-verbs.json and cannot drift).
//
// The operator: "I think we could also liven up the conversations by rotating through a series of words
// that means 'orchicon is thinking' but variations like 'inquisiting, contemplating, planning, etc.' that
// changes every few seconds. We should have a ton of them."
//
// THE INDEX IS A PURE FUNCTION OF SERVER TIME. verbAt takes the timestamp the server already puts on the
// wire — Heartbeat.server_time_unix_ms (emitted at internal/askorchicon/chat.go, whose own doc says it is
// there so a client can measure socket age/skew). Two clients holding the same server stamp draw the SAME
// word, with no shared state, no new RPC and no server-side counter; and because the client never consults
// its own wall clock for the index, a skewed clock still draws the same word (the callers add only a DELTA
// since their own receipt instant, so the word keeps advancing between heartbeats).

export const VERB_PERIOD_MS = 4000;

/**
 * The widest entry the one-row footer will accept. Asserted against the list, not merely documented
 * (ask-verbs.test.ts), so an entry that does not fit fails a test rather than clipping a footer.
 */
export const VERB_CELL_CAP = 14;

// "A ton of them": 72 entries, all present participles that read as thinking or working, every one a
// grammatical continuation of "Orchicon is …".
//
// SHAPE IS A CONSTRAINT, NOT A PREFERENCE: single lowercase a-z words of at most VERB_CELL_CAP characters —
// no spaces, no punctuation, no emoji, nothing the TUI's `ascii` theme could not draw. And NOTHING IS A
// LIVENESS CLAIM: the rotation says what the turn is DOING to the problem, never that the stream is
// receiving or the work is progressing, because a stalled stream would make such a word false.
//
// THIS LITERAL IS PINNED to ask-verbs.json, byte for byte and in ORDER, by ask-verbs.test.ts — the same
// one-fixture-both-languages binding the Schedules page uses (schedules-fixture.json), which the Go mirror
// reads too.
const VERBS_RAW =
  "thinking,pondering,contemplating,considering,deliberating," +
  "musing,ruminating,cogitating,reasoning,reflecting," +
  "meditating,mulling,brooding,speculating,hypothesizing," +
  "theorizing,analyzing,scrutinizing,examining,inspecting," +
  "investigating,inquiring,inquisiting,questioning,probing," +
  "exploring,researching,studying,surveying,scouting," +
  "mapping,charting,plotting,planning,scheming," +
  "strategizing,devising,designing,drafting,sketching," +
  "outlining,organizing,arranging,sorting,sifting," +
  "parsing,dissecting,unraveling,untangling,synthesizing," +
  "assembling,composing,crafting,constructing,shaping," +
  "refining,polishing,honing,pruning,distilling," +
  "conjuring,calculating,computing,enumerating,estimating," +
  "weighing,balancing,verifying,validating,rehearsing," +
  "preparing,calibrating";

export const ASK_VERBS: string[] = VERBS_RAW.split(",");

/**
 * The pure selector: the same timestamp always yields the same word, on any client, in any process. No
 * clock, no RNG, no mutable state — integer division and modulo only, so this and Go's VerbAt cannot
 * disagree on a result.
 *
 * t <= 0 (or a non-finite number) IS THE "NO HEARTBEAT YET" SENTINEL — a real server stamp is Unix
 * milliseconds and always positive — and it returns ASK_VERBS[0]. That is the fallback for the moment
 * right after the operator sends, which is exactly when they are most likely to be looking: the line must
 * never be empty and must never throw.
 */
export function verbAt(serverTimeUnixMs: number): string {
  if (!Number.isFinite(serverTimeUnixMs) || serverTimeUnixMs <= 0) {
    return ASK_VERBS[0];
  }
  return ASK_VERBS[Math.floor(serverTimeUnixMs / VERB_PERIOD_MS) % ASK_VERBS.length];
}

/**
 * The accessibility off-switch, and it is part of the feature rather than a follow-up: the rotation IS
 * animation.
 *
 * MECHANISM: the profile's own prefers-reduced-motion: reduce. That is the setting the operator already
 * has (unlike a bespoke app preference), it needs no persistence, and it is cheap to assert. A non-DOM
 * environment (unit tests, SSR) has no such preference and rotation stays ON, which is the useful default.
 */
export function verbRotationOn(): boolean {
  if (typeof window === "undefined" || typeof window.matchMedia !== "function") {
    return true;
  }
  try {
    return !window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  } catch {
    return true;
  }
}

/**
 * What the activity line asks for: the rotating word for this server time, or the still first entry when
 * the operator has reduced motion on. Keeping the off-switch here (rather than at the call site) means one
 * code path produces the word, and both the pre-heartbeat fallback and the disabled case land on
 * ASK_VERBS[0] — "the line is present and says something true" is the same promise in both.
 */
export function activityVerb(serverTimeUnixMs: number): string {
  return verbRotationOn() ? verbAt(serverTimeUnixMs) : ASK_VERBS[0];
}

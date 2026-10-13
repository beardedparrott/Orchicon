// chat-scroll.ts — the pure scroll-decision helpers behind the transcript's
// jump-to-top / jump-to-latest controls, and behind the live-stick rule that keeps a
// streaming reply pinned to the tail.
//
// WHY THESE LIVE OUTSIDE THE COMPONENT. The controls' behaviour is "am I at the
// bottom (so live content should pin me)?" and "am I at the top (so there is nothing
// above to jump to)?" — arithmetic over the element's metrics. Kept as pure functions,
// that arithmetic is unit-testable without a DOM (the repo's component tests render
// server-side, so a live scroll cannot be simulated); inlined in the component it would
// be reachable only through a browser.

export interface ScrollMetrics {
  scrollTop: number;
  scrollHeight: number;
  clientHeight: number;
}

/**
 * metricsOf reads the three numbers every scroll decision is made from.
 *
 * Structural rather than HTMLElement so the module stays a pure library (it is unit-tested
 * without a DOM), and shared rather than re-derived per component: two surfaces reading the
 * same element and disagreeing about the arithmetic is how the transcript and the session
 * pane drifted apart in the first place.
 */
export function metricsOf(el: Pick<ScrollMetrics, "scrollTop" | "scrollHeight" | "clientHeight">): ScrollMetrics {
  return {
    scrollTop: el.scrollTop,
    scrollHeight: el.scrollHeight,
    clientHeight: el.clientHeight,
  };
}

/** STICK_THRESHOLD_PX is how close to the bottom still counts as "at the bottom". */
export const STICK_THRESHOLD_PX = 80;

/** distanceFromBottom is the pixels of content below the viewport's bottom edge. */
export function distanceFromBottom(m: ScrollMetrics): number {
  return m.scrollHeight - m.scrollTop - m.clientHeight;
}

/**
 * isNearBottom reports whether the view is close enough to the tail that a new
 * message should keep it pinned there — the live-stick rule for a streaming
 * reply. This is the exact predicate the container used before it was extracted.
 */
export function isNearBottom(m: ScrollMetrics, threshold = STICK_THRESHOLD_PX): boolean {
  return distanceFromBottom(m) < threshold;
}

/**
 * AT_BOTTOM_EPSILON_PX absorbs sub-pixel rounding so "at the bottom" is reachable at all.
 * A browser's scrollTop is a float after a device-pixel-ratio scale, so a view that is
 * visually at the tail can measure a fraction of a pixel short of it.
 */
export const AT_BOTTOM_EPSILON_PX = 1;

/**
 * atBottom reports whether the view is AT the tail — not merely near it.
 *
 * WHY THIS EXISTS ALONGSIDE isNearBottom, because the two look like the same question and
 * are not. They answer the two halves of the live-stick rule:
 *
 *   - WHILE FOLLOWING, the margin is right. A view pinned to the tail must stay pinned
 *     through a few pixels of slack; requiring the exact tail would drop the follow on any
 *     rounding or tiny content change, which is the opposite failure.
 *   - WHILE NOT FOLLOWING, re-acquiring needs the ACTUAL tail. The operator's own words:
 *     "This should only do this if you are ACTIVELY at the bottom."
 *
 * Using the margin for the second half is the rubberband. A deliberate upward gesture
 * releases the stick, and then the very scroll that gesture caused is still inside the
 * margin — so a position test re-arms the stick immediately, the next chunk snaps the view
 * back to the tail, and the operator cannot leave. On a trackpad, whose wheel events are a
 * few pixels each, no single gesture can ever get outside the margin, so the view is pinned
 * no matter how hard they scroll.
 */
export function atBottom(m: ScrollMetrics, epsilon = AT_BOTTOM_EPSILON_PX): boolean {
  return distanceFromBottom(m) <= epsilon;
}

/**
 * isNearTop reports whether the view is close enough to the first message that a
 * jump-to-top control has nothing to do. Content ABOVE the fold is what makes the
 * control useful, so this is the same margin measured from the top.
 */
export function isNearTop(m: ScrollMetrics, threshold = STICK_THRESHOLD_PX): boolean {
  return m.scrollTop < threshold;
}

/** bottomScrollTop is the scrollTo target that lands on the latest message. */
export function bottomScrollTop(m: Pick<ScrollMetrics, "scrollHeight">): number {
  return m.scrollHeight;
}

/** topScrollTop is the scrollTo target that lands on the first message. */
export function topScrollTop(): number {
  return 0;
}

/**
 * ScrollEnd names which end of the transcript a scroll is being driven toward.
 *
 * IT REPLACES A TIME WINDOW, and that replacement is the whole fix. The container used to
 * ignore any scroll event arriving within 200ms of its own auto-scroll, on the theory that
 * such an event was probably its own. But an event it suppressed was an event whose
 * OPERATOR INTENT it never recorded — so a wheel-up landing inside that window left the
 * live stick ON, and the next streamed chunk dragged the operator back to the tail. During
 * a streaming reply auto-scrolls happen continuously, so that window was open most of the
 * time, which is why the report reads as "scrolling up always snaps me back".
 *
 * AN END RATHER THAN A PIXEL TARGET, deliberately. A recorded position goes stale the
 * moment the content grows — and content grows continuously while a reply streams, so a
 * stale target is the normal case rather than an edge one. "We are driving toward the
 * bottom" stays true however much arrives, so there is nothing to go stale.
 */
export type ScrollEnd = "bottom" | "top";

/**
 * hasArrived reports whether a scroll event is one WE drove, now resting at the end we
 * sent it to.
 *
 * False means the event is still OURS — an animation in flight — and carries no operator
 * intent, so reading a stick decision out of it would be reading the component's own
 * motion as the operator's wish. This is the case that made "jump to top" fail: a smooth
 * scroll from the tail passes back THROUGH the near-bottom margin on its way up, so an
 * event read mid-flight says "near the bottom" and re-arms the stick the jump had just
 * released — and the next chunk pulls the operator straight back down.
 */
export function hasArrived(m: ScrollMetrics, end: ScrollEnd): boolean {
  return end === "bottom" ? isNearBottom(m) : isNearTop(m);
}

/**
 * nextStick decides the live stick after a scroll event, given whether it was ON before.
 *
 * THE ASYMMETRY IS THE WHOLE POINT, and it is why this is a function rather than a
 * condition written at each call site: the two states ask DIFFERENT questions of the same
 * position.
 *
 *   - ALREADY FOLLOWING → isNearBottom. The margin absorbs slack, so the follow survives
 *     rounding and tiny content changes.
 *   - NOT FOLLOWING → atBottom. The ACTUAL tail, because re-acquiring on the margin is the
 *     rubberband: a deliberate wheel-up leaves the view inside the margin, so a
 *     margin-based test re-arms the stick on the very scroll that gesture caused and the
 *     next chunk snaps the view back down.
 *
 * The operator's own words for the second half: "This should only do this if you are
 * ACTIVELY at the bottom."
 */
export function nextStick(m: ScrollMetrics, wasSticking: boolean): boolean {
  return wasSticking ? isNearBottom(m) : atBottom(m);
}

/**
 * UPWARD_KEYS are the navigation keys that mean "move AWAY from the tail". They are the
 * keyboard half of the release rule below; DOWNWARD_KEYS is its other half, named so the
 * two directions cannot drift apart.
 */
export const UPWARD_KEYS: ReadonlySet<string> = new Set(["Home", "PageUp", "ArrowUp"]);

/** DOWNWARD_KEYS mean "move TOWARD the tail" — they never release the stick. */
export const DOWNWARD_KEYS: ReadonlySet<string> = new Set([
  "End",
  "PageDown",
  "ArrowDown",
  " ",
  "Spacebar",
]);

/**
 * wheelReleasesStick reports whether a wheel gesture is a deliberate move away from the
 * tail (any upward delta).
 *
 * WHY A GESTURE RELEASES, RATHER THAN ONLY A POSITION. Releasing on position alone needs
 * a scroll event to happen, and an upward gesture can produce none at all: the element
 * may already be at the top, or the gesture may arrive in the same tick as an appended
 * message that re-pinned it. Both are realistic while a reply is streaming — which is
 * exactly when the operator reaches for the wheel — and in both the stick would stay on.
 *
 * READING THE GESTURE IS NOT A GUESS ABOUT POSITION: an upward wheel is unambiguous
 * operator intent, and it is the operator's intent that the stick encodes.
 */
export function wheelReleasesStick(deltaY: number): boolean {
  return deltaY < 0;
}

/** keyReleasesStick reports whether a navigation key means "move away from the tail". */
export function keyReleasesStick(key: string): boolean {
  return UPWARD_KEYS.has(key);
}

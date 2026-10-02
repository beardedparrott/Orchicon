// railResize — pure geometry for the drag-resizable diff rail.
//
// No React and no DOM here on purpose: the clamp that protects the CHAT
// column is the acceptance-critical rule of this feature ("no squeezed-out
// chat, no horizontal page scroll"), so it lives in a function that can be
// unit-tested without a browser (this repo has no jsdom — see
// BuildLogViewer.test.tsx / AskCard.test.tsx for the established idiom).
//
// The rail is the FIRST flex child of a row with the chat column beside it
// (ask-orchicon.tsx row, executions_.$id.tsx row), so a wider rail is
// literally taken out of the chat's width. Therefore the maximum is not a
// constant alone: it is min(RAIL_MAX_WIDTH, containerWidth - MIN_CHAT_WIDTH).

/** Width the rail opens at, and the double-click/kb-reset target. */
export const RAIL_DEFAULT_WIDTH = 480;

/** Below this the diff body (side-by-side, two 240px panes) stops being readable. */
export const RAIL_MIN_WIDTH = 320;

/** Beyond this a "side-by-side" rail is just a stretched single column. */
export const RAIL_MAX_WIDTH = 960;

/** Width the CHAT column beside the rail must keep to stay usable. */
export const MIN_CHAT_WIDTH = 360;

/** One keyboard step (ArrowLeft/ArrowRight on the handle). */
export const RAIL_STEP = 24;

/**
 * The largest rail width the CONTAINER can afford right now.
 *
 * `containerWidth <= 0` (not measured yet, or a host that passed no ref) falls
 * back to the constant ceiling — never to "unbounded", so an unmeasured host
 * still cannot produce `width: NaN` in the inline style.
 */
export function maxRailWidth(containerWidth: number): number {
  if (!Number.isFinite(containerWidth) || containerWidth <= 0) return RAIL_MAX_WIDTH;
  return Math.max(RAIL_MIN_WIDTH, Math.min(RAIL_MAX_WIDTH, containerWidth - MIN_CHAT_WIDTH));
}

/**
 * Clamp a desired rail width so that (a) the rail stays readable and (b) the
 * chat column beside it keeps at least MIN_CHAT_WIDTH — the actual fix for the
 * old `min-w-[480px]` behaviour, where a narrow page squeezed the chat instead
 * of the rail.
 */
export function clampRailWidth(width: number, containerWidth: number): number {
  const max = maxRailWidth(containerWidth);
  const w = Number.isFinite(width) ? width : RAIL_DEFAULT_WIDTH;
  return Math.max(RAIL_MIN_WIDTH, Math.min(max, w));
}

/**
 * Keyboard step: `direction` is +1 to widen (ArrowRight — the handle moves
 * right) or -1 to narrow (ArrowLeft), both clamped by the container.
 */
export function stepRailWidth(
  current: number,
  direction: -1 | 1,
  containerWidth: number,
): number {
  return clampRailWidth(current + direction * RAIL_STEP, containerWidth);
}

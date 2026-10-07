// railResize — pure geometry for the drag-resizable diff rail.
//
// No React and no DOM here on purpose: the clamp that protects the CHAT
// column is the acceptance-critical rule of this feature ("no squeezed-out
// chat, no horizontal page scroll"), so it lives in a function that can be
// unit-tested without a browser (this repo has no jsdom — see
// BuildLogViewer.test.tsx / AskCard.test.tsx for the established idiom).
//
// The rail is the FIRST flex child of a row that ALSO holds the chat column
// AND (on the Ask page) a fixed right conversations panel. A wider rail is
// literally taken out of the row, so the maximum is not a constant alone: it
// is min(RAIL_MAX_WIDTH, containerWidth - reserved), where `reserved` is the
// width the row must keep for EVERYTHING that is not the rail — the chat
// column (MIN_CHAT_WIDTH) plus any FIXED siblings the row also renders (the
// 288px conversations panel) plus the flex gaps between them. Reserving only
// MIN_CHAT_WIDTH was the actual cause of the squeeze: on the Ask page the row
// is [rail | chat | 288px panel], so a ceiling of containerWidth - 360 left
// the chat with just containerWidth - rail - 288 — 72px at the 888px max.

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
 * `reserved` is the total width the row must keep for everything that is not
 * the rail (the chat column + any fixed siblings + gaps). It defaults to
 * MIN_CHAT_WIDTH for a bare [rail | chat] row.
 *
 * `containerWidth <= 0` (not measured yet, or a host that passed no ref) falls
 * back to the constant ceiling — never to "unbounded", so an unmeasured host
 * still cannot produce `width: NaN` in the inline style.
 */
export function maxRailWidth(containerWidth: number, reserved: number = MIN_CHAT_WIDTH): number {
  if (!Number.isFinite(containerWidth) || containerWidth <= 0) return RAIL_MAX_WIDTH;
  const r = Number.isFinite(reserved) && reserved > 0 ? reserved : MIN_CHAT_WIDTH;
  return Math.max(RAIL_MIN_WIDTH, Math.min(RAIL_MAX_WIDTH, containerWidth - r));
}

/**
 * Clamp a desired rail width so that (a) the rail stays readable and (b) the
 * chat column beside it keeps at least MIN_CHAT_WIDTH while every FIXED
 * sibling the row also renders keeps its width — the actual fix for the old
 * `min-w-[480px]` behaviour, where a narrow page squeezed the chat instead of
 * the rail.
 */
export function clampRailWidth(
  width: number,
  containerWidth: number,
  reserved: number = MIN_CHAT_WIDTH,
): number {
  const max = maxRailWidth(containerWidth, reserved);
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
  reserved: number = MIN_CHAT_WIDTH,
): number {
  return clampRailWidth(current + direction * RAIL_STEP, containerWidth, reserved);
}

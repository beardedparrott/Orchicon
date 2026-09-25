// ask-consent.ts — the pure state machine behind the consent card.
//
// WHY THIS IS NOT A StreamItem. The live turn's `items` array is emptied by
// every turn-lifecycle updater in the route (slot re-arm, completion, failure),
// which is right for chunks and wrong for an ask: the acceptance criteria
// require the OUTCOME to stay in the transcript ("an operator scrolling back
// must see that a grant was given and what it covered"). So asks live in their
// own array that no turn-lifecycle updater clears, and this module owns its
// rules — dedupe, resolve-in-place, and what each outcome reads as.

import {
  PermissionChoice,
  type PermissionAsk,
} from "@/api/gen/orchicon/api/v1/ask_orchicon_service_pb";

/** The operator's answer to one ask, or the server's expiry of it. */
export type AskOutcome =
  | { kind: "allow_once" }
  | { kind: "allow_session" }
  | { kind: "deny" }
  | { kind: "expired"; detail?: string };

/** One ask as the transcript holds it: the wire ask plus its settlement. */
export interface AskItem {
  /** ask.key is the ask id — the dedupe key across the live and watch sockets. */
  key: string;
  ask: PermissionAsk;
  /** outcome is null while the ask is still awaiting an answer. */
  outcome: AskOutcome | null;
  resolvedAt: number | null;
}

/**
 * applyAskChunk folds one streamed ask into the list. DEDUPE BY ASK ID: the
 * live socket and the watch socket can both deliver the same ask (and the watch
 * re-dials after a refresh), so a repeat is a no-op rather than a second card.
 * An ask that is already resolved is never resurrected.
 */
export function applyAskChunk(items: AskItem[], ask: PermissionAsk): AskItem[] {
  const key = ask.askId;
  if (!key) return items;
  if (items.some((i) => i.key === key)) return items;
  return [...items, { key, ask, outcome: null, resolvedAt: null }];
}

/**
 * resolveAsk settles one ask IN PLACE (its position in the transcript is where
 * the question was asked, so the card must not jump to the end) and is a no-op
 * for an ask that is already settled — a late second decision never overwrites
 * the first, which is what the server's `applied: false` reply means.
 */
export function resolveAsk(
  items: AskItem[],
  askID: string,
  outcome: AskOutcome,
  at: number = Date.now(),
): AskItem[] {
  return items.map((i) =>
    i.key === askID && i.outcome === null
      ? { ...i, outcome, resolvedAt: at }
      : i,
  );
}

/** pendingFor returns the asks still awaiting an answer, oldest first. */
export function pendingFor(items: AskItem[] | undefined): AskItem[] {
  if (!items) return [];
  return items.filter((i) => i.outcome === null);
}

/** outcomeFromChoice maps a decision the operator made to its outcome. */
export function outcomeFromChoice(choice: PermissionChoice): AskOutcome {
  switch (choice) {
    case PermissionChoice.ALLOW_SESSION:
      return { kind: "allow_session" };
    case PermissionChoice.DENY:
      return { kind: "deny" };
    default:
      return { kind: "allow_once" };
  }
}

/**
 * askTargetLabel is the target the card names: the command for a bash ask, the
 * paths for a write/edit — never an opaque id. The server's `summary` already
 * carries it; this is the fallback for a card whose summary is missing.
 */
export function askTargetLabel(ask: PermissionAsk): string {
  if (ask.summary) return ask.summary;
  if (ask.command) return `${ask.tool}: ${ask.command}`;
  if (ask.targets.length > 0) return `${ask.tool} ${ask.targets.join(", ")}`;
  return ask.tool || "a tool permission request";
}

/**
 * outcomeLabel is the TRANSCRIPT line for a settled ask. It states what was
 * decided and what the decision covered: a session grant reports the directory
 * it opens, so "what was granted" is readable without opening anything.
 */
export function outcomeLabel(ask: PermissionAsk, outcome: AskOutcome): string {
  const target = askTargetLabel(ask);
  switch (outcome.kind) {
    case "allow_once":
      return `Allowed once — ${target}`;
    case "allow_session":
      return ask.directory
        ? `Allowed for this session — ${target} (covers ${ask.directory})`
        : `Allowed for this session — ${target}`;
    case "deny":
      return `Denied — ${target}`;
    default:
      return outcome.detail
        ? `Expired unanswered — ${target} (${outcome.detail})`
        : `Expired unanswered — ${target}`;
  }
}

/**
 * popoverNudge is how far a disclosure panel must be pushed RIGHT so its left
 * edge does not run off the screen, given the anchor's right edge and the
 * panel's width.
 *
 * The session-grants panel is a fixed-width (320px) popover anchored `right-0`
 * to a trigger that sits MID-header, with the header's right-side chrome
 * between it and the viewport edge. On a narrow viewport (375px) a 320px panel
 * therefore starts ~30px OFF the left edge and clips the granted DIRECTORY —
 * the one thing that list exists to show. 0 when it already fits (the desktop
 * case), so the anchored layout is untouched where there is room.
 */
export function popoverNudge(
  anchorRight: number,
  panelWidth: number,
  inset = 8,
): number {
  const left = anchorRight - panelWidth;
  return left < inset ? Math.round(inset - left) : 0;
}

/**
 * relativeGrantAge renders "just now" / "3m ago" / "2h ago" from a Unix-seconds
 * grant time. 0 (unknown) renders an empty string rather than "1970".
 */
export function relativeGrantAge(grantedAtUnix: number, now: number = Date.now()): string {
  if (!grantedAtUnix) return "";
  const secs = Math.max(0, Math.floor(now / 1000 - grantedAtUnix));
  if (secs < 45) return "just now";
  const mins = Math.floor(secs / 60);
  if (mins < 60) return `${Math.max(1, mins)}m ago`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) return `${hours}h ago`;
  return `${Math.floor(hours / 24)}d ago`;
}

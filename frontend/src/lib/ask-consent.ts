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
  /**
   * at is when the ask ARRIVED, in epoch ms. It is what places the card in the
   * transcript: a consent card is a block in the conversation, not an appendix,
   * so it is interleaved with the messages by time and moves up as the
   * conversation grows.
   *
   * Required, and NOT optional with a default, because the bug this fixes was
   * precisely a missing timestamp: the cards were rendered as a separate list
   * AFTER every message, so a settled card sat pinned at the bottom of the
   * transcript forever (the operator: "the permission blocks in the GUI are still
   * remaining at the bottom at the end of a turn which makes no sense. They
   * should be in the conversation and move up just like any other conversation
   * block").
   */
  at: number;
}

/**
 * applyAskChunk folds one streamed ask into the list. DEDUPE BY ASK ID: the
 * live socket and the watch socket can both deliver the same ask (and the watch
 * re-dials after a refresh), so a repeat is a no-op rather than a second card.
 * An ask that is already resolved is never resurrected.
 */
export function applyAskChunk(
  items: AskItem[],
  ask: PermissionAsk,
  at: number = Date.now(),
): AskItem[] {
  const key = ask.askId;
  if (!key) return items;
  if (items.some((i) => i.key === key)) return items;
  return [...items, { key, ask, outcome: null, resolvedAt: null, at }];
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

/**
 * blockAt is the epoch-ms a transcript BLOCK sorts by: a streamed message, an
 * optimistic echo, a live chunk, or a consent card.
 *
 * `at` is the ask's arrival time for AskItem and a message's createdAt
 * otherwise. Ordering is what places a consent card BETWEEN the messages it
 * happened between, rather than after all of them.
 */
export function blockAt(block: { kind: "ask" } | { at: number }): number {
  return "kind" in block && block.kind === "ask"
    ? (block as unknown as AskItem).at
    : (block as { at: number }).at;
}

/**
 * interleave merges the durable/renderable transcript with the conversation's
 * consent cards, ordered by time.
 *
 * A card whose arrival time is unknown (0 — a shape from an older client) sorts
 * FIRST, which is the same wrong end the missing timestamp produced. That is
 * deliberate: it makes the failure visible in a test rather than silently pinning
 * the card to the bottom.
 */
export function interleave<T extends { at: number }>(
  blocks: T[],
  asks: AskItem[] | undefined,
): Array<{ seq: number; at: number; block: T | AskItem; isAsk: boolean }> {
  const out: Array<{ seq: number; at: number; block: T | AskItem; isAsk: boolean }> = [];
  blocks.forEach((b, seq) => out.push({ seq, at: b.at, block: b, isAsk: false }));
  (asks ?? []).forEach((a, seq) =>
    out.push({ seq, at: a.at, block: a, isAsk: true }),
  );
  // Stable by construction: the seq tiebreak keeps same-instant blocks in their
  // source order, so a message and a card at the same ms do not swap on a poll.
  out.sort((x, y) => x.at - y.at || (x.isAsk === y.isAsk ? x.seq - y.seq : x.isAsk ? 1 : -1));
  return out;
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
 * The SESSION row's label template. The row is NOT a fixed string, because what it
does depends on WHICH directory it covers and the operator is entitled to see that
before they choose it.
 *
 * WHY IT NAMES THE SCOPE. The row used to read "Allow for this session", and the
operator — asked for the same directory over and over, having no idea how far a
session grant reached — asked for "an option that says something along the lines of
'Never ask again for this directory for this session'". The option ALREADY existed and
already worked that way (a grant covers the directory and everything under it, for the
conversation); what it did not do was SAY so. A consent row whose reach the operator
has to guess is a row they will not use.
 *
 * NEITHER PIECE IS DECORATION: the TUI carries the same three literals verbatim (the
repo's parity rule — TestConsentWordingMatchesTheGUISource scans this tree for them),
so the two clients describe the same decision identically.
 */
export const CONSENT_SESSION_PREFIX = "Never ask again in ";
export const CONSENT_SESSION_SUFFIX = " this session";
/**
 * CONSENT_SESSION_NO_DIR is the label when the ask names no directory (a detail that
never resolved). It says "this directory" rather than borrowing the target, because the
target is a FILE for a write ask and naming a file as the directory a grant covers
would be a lie about the scope.
 */
export const CONSENT_SESSION_NO_DIR = "Never ask again in this directory this session";

/**
 * sessionLabel is the session row's text, naming the directory the grant would cover.
 * It and the TUI's PermissionAsk.SessionLabel must produce the same string for the same
 * ask.
 */
export function sessionLabel(ask: { directory?: string }): string {
  const dir = (ask.directory ?? "").trim();
  return dir
    ? `${CONSENT_SESSION_PREFIX}${dir}${CONSENT_SESSION_SUFFIX}`
    : CONSENT_SESSION_NO_DIR;
}

/**
 * outcomeFromWire maps the server's PermissionAskResolved.outcome onto the local
 * AskOutcome. It exists because a decision can be made in ANOTHER client: an ask reaches
 * every watcher of a turn while only the answering client cleared its own copy, so the
 * collector publishes the outcome and every watcher settles from it.
 *
 * An UNRECOGNISED outcome becomes `expired` with the raw value in the detail rather than
 * silently claiming `allow_once` — reporting a permission as granted on the strength of a
 * value we did not understand is the one failure a consent UI must not have.
 */
export function outcomeFromWire(outcome: string): AskOutcome {
  switch (outcome) {
    case "allow_once":
      return { kind: "allow_once" };
    case "allow_session":
      return { kind: "allow_session" };
    case "deny":
      return { kind: "deny" };
    case "answered":
      // A question's answer is content, not a permission outcome. It settles the card
      // the same way; the card itself shows the answer.
      return { kind: "allow_once" };
    case "expired":
      return { kind: "expired" };
    default:
      return { kind: "expired", detail: `unrecognised outcome "${outcome}"` };
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
      // The RECORD uses the row's vocabulary, so scrolling back reads as the choice that
      // was actually made rather than as a second, differently-named outcome.
      return ask.directory
        ? `Never asking again in ${ask.directory} this session — ${target}`
        : `Never asking again this session — ${target}`;
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

/**
 * outcomeFromRecord maps a PERSISTED `permission.*` record onto the outcome it represents, or
 * null when the record is not a resolution of a card.
 *
 * THIS IS THE DURABLE HALF OF CROSS-CLIENT SETTLING, and it exists because the live stream
 * event is not enough — which is why the operator kept seeing the same bug come back after
 * being fixed "in the stream" repeatedly. A `PermissionAskResolved` event reaches only a
 * client watching the turn RIGHT THEN: a second tab, another device, or the same page after a
 * reload never sees it, and asking the operator to refresh is not a fix.
 *
 * The server writes every consent decision into the turn's ledger as a synthetic
 * `permission.<verdict>` record, and that record ID is the ADAPTER'S ASK ID (see
 * toolLedger.recordPermission). The ledger is part of the assistant message in the database,
 * so it IS server truth: readable by any client, at any time, across a reload. A client
 * holding a card looks its ask up here and settles.
 *
 * The vocabulary is the verdict names Verdict.String() emits, plus the `user_<CHOICE>` form
 * the answer path records. Records that are NOT a resolution return null and settle nothing:
 * `permission.ask` is the card itself (settling on it would clear a card the instant it was
 * raised), and `session_grant`/`accept`/`project`/`fullsend` describe a call that proceeded
 * WITHOUT a card, so there is nothing on screen to settle.
 */
export function outcomeFromRecord(functionName: string): AskOutcome | null {
  switch (functionName) {
    case "permission.user_PERMISSION_CHOICE_ALLOW_ONCE":
      return { kind: "allow_once" };
    case "permission.user_PERMISSION_CHOICE_ALLOW_SESSION":
      return { kind: "allow_session" };
    case "permission.user_PERMISSION_CHOICE_DENY":
      return { kind: "deny" };
    case "permission.expired":
      return { kind: "expired", detail: "unanswered at turn end" };
    // A refused call: the operator never saw a card for these, but a card CAN have been
    // raised first (an ask that was then denied at the layer), and the call did not run —
    // so the honest settlement is a denial rather than leaving the card live.
    case "permission.deny":
    case "permission.never_allow":
      return { kind: "deny" };
    // A policy that could not be read is NOT a decision: nothing was approved and nothing
    // was refused, so the card is settled as expired rather than as a denial the operator
    // never made.
    case "permission.policy_error":
      return { kind: "expired", detail: "the permission policy could not be read" };
    // A question's answer is CONTENT, not a permission outcome; it settles the card the same
    // way, exactly as outcomeFromWire treats it.
    case "permission.answered":
      return { kind: "allow_once" };
    default:
      return null;
  }
}

/** One message as the reconciliation reads it: just the tool calls and their results. */
export interface LedgerMessage {
  toolCalls?: ReadonlyArray<{ id: string; functionName: string }> | null;
}

/**
 * settleFromLedger settles any card whose ask the transcript already RESOLVED, and is the
 * backstop that makes a card's state survive a reload, a second tab, or another device.
 *
 * IT RETURNS THE SAME REFERENCE WHEN NOTHING CHANGED. That is not a micro-optimisation: this
 * runs from a render effect on every transcript poll, and `resolveAsk` always returns a new
 * array (it is a `map`), so returning that unconditionally would schedule a state update on
 * every poll forever. Comparing first is what keeps the effect silent when there is nothing
 * to do.
 */
export function settleFromLedger(
  items: AskItem[],
  messages: ReadonlyArray<LedgerMessage> | undefined,
): AskItem[] {
  if (items.length === 0 || !messages || messages.length === 0) return items;
  const resolved = new Map<string, AskOutcome>();
  for (const m of messages) {
    for (const c of m.toolCalls ?? []) {
      if (!c.functionName.startsWith("permission.")) continue;
      const outcome = outcomeFromRecord(c.functionName);
      // First resolution wins, mirroring resolveAsk: a later record for the same ask must not
      // overwrite what actually happened to it.
      if (outcome && !resolved.has(c.id)) resolved.set(c.id, outcome);
    }
  }
  if (resolved.size === 0) return items;
  let changed = false;
  const next = items.map((i) => {
    if (i.outcome !== null) return i;
    const outcome = resolved.get(i.key);
    if (!outcome) return i;
    changed = true;
    return { ...i, outcome, resolvedAt: i.resolvedAt ?? Date.now() };
  });
  return changed ? next : items;
}

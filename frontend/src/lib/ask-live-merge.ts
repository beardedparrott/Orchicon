// ask-live-merge.ts — WHICH OF THE TWO COPIES OF THE RUNNING REPLY IS DRAWN.
//
// THE SAME TEXT ARRIVES TWICE, AND THAT IS BY DESIGN. While a turn runs, the server both
//   (a) streams the reply as it is written (TextChunk / Reasoning), and
//   (b) upserts the turn's collected text into the ACKED assistant row every 250ms (the partial
//       MIRROR) so a client that lost the stream can watch the reply grow through ListMessages.
// See internal/askorchicon/chat.go (the delta arm + upsertPartialMessage).
//
// A transcript that draws both shows the reply TWICE — measured in a real browser against the
// shared fixture plane (which emits TextChunk AND seeds the mirror row): two <p> elements, each
// carrying the whole reply. Drawing one of them is therefore a decision, not a rendering detail.
//
// THE STREAM WINS WHILE IT IS LIVE, for the reason the mirror exists at all: it is the same text
// arriving sooner, chunk by chunk, so it is the copy that can actually grow. The mirror row is the
// FALLBACK — a client with no live stream (a reload, a second tab, a turn started elsewhere) has
// empty live items and draws the row, which is exactly how that case was already handled. When the
// turn ends the live items are cleared, the row becomes the only copy, and the finalize has written
// the complete reply into it.
//
// THIS IS A SEAM, NOT A HIDDEN ROW: the row is suppressed ONLY while it is the in-flight one
// (`pendingReplyId`, the id TurnStarted acked) AND this client holds live text/reasoning to show
// instead. A completed turn has no live items, so the durable transcript is never withheld.

export interface MirrorRowMessage {
  id: string;
  /**
   * toolCalls is what makes the mirror row more than a copy of the live text.
   *
   * THE BUG THIS FIELD FIXES. The supersession rule drops the in-flight row while live items are drawing
   * the reply, and that was right for TEXT — the live copy is the same text, drawn sooner. But the row also
   * carries the turn's TOOL CALLS, and a recorded `ask_user` call is what renders the clarifying card
   * (MessageBubble: `message.toolCalls.find(isAskUserToolCall)`). So dropping the row dropped the card:
   * the operator saw "Orchicon is weighing… · no output for 123s" with a card sitting unrendered, and a
   * page reload showed it — because a fresh load has NO live items, so the row (and its card) rendered.
   * Reloading was not "refreshing stale state"; it was the only path that drew the row at all.
   *
   * The live stream does not carry tool calls, so nothing else can render them. The row is therefore kept
   * whenever it carries any.
   */
  toolCalls?: readonly unknown[] | null;
}

/**
 * mirrorRowSuperseded answers whether the in-flight assistant row should be SKIPPED because the
 * live stream is already drawing the same reply.
 *
 * @param messageId     the row being considered for the transcript
 * @param pendingReplyId the acked in-flight assistant message id (null when no turn is known)
 * @param isStreaming   this client's own live slot for the conversation
 * @param hasLiveItems  this client holds live text/reasoning items to render instead
 */
export function mirrorRowSuperseded(
  message: MirrorRowMessage,
  pendingReplyId: string | null,
  isStreaming: boolean,
  hasLiveItems: boolean,
): boolean {
  if (!isStreaming) return false; // no live stream here: the row IS the view (reload / other tab)
  if (!pendingReplyId) return false; // no acked id: cannot tell which row is the mirror
  if (message.id !== pendingReplyId) return false; // only the in-flight row is superseded
  // No live items to draw would make this a BLANK turn, so the mirror is kept as the fallback.
  if (!hasLiveItems) return false;
  // AND A ROW CARRYING TOOL CALLS IS NEVER SUPERSEDED, even though its TEXT is a duplicate of the live
  // copy. The live stream carries text and reasoning only — never tool calls — so this row is the ONLY
  // thing that can render the turn's card, its tool ledger, or a recorded ask_user call. Dropping it for
  // the sake of the text would take those with it, which is exactly what happened: the card existed in
  // the transcript and could not be drawn, and only a reload (which has no live items) revealed it.
  if ((message.toolCalls ?? []).length > 0) return false;
  return true;
}

/**
 * dropSupersededMirror removes the in-flight assistant row from a transcript when the live stream
 * is already drawing it, so the reply appears exactly once. Order and every other row are
 * untouched.
 */
export function dropSupersededMirror<T extends MirrorRowMessage>(
  messages: readonly T[],
  pendingReplyId: string | null,
  isStreaming: boolean,
  hasLiveItems: boolean,
): T[] {
  if (!isStreaming || !hasLiveItems || !pendingReplyId) return [...messages];
  return messages.filter((m) => !mirrorRowSuperseded(m, pendingReplyId, isStreaming, hasLiveItems));
}

/** hasLiveTextItems reports whether the grouped stream holds text/reasoning worth drawing. */
export function hasLiveTextItems(items: readonly { kind: string }[]): boolean {
  return items.some((i) => i.kind === "text" || i.kind === "reasoning");
}

/**
 * mirrorRowProseLifted answers whether the in-flight row's PROSE should be skipped because the live
 * stream is already drawing it — while the row ITSELF stays in the transcript.
 *
 * THE BUG THIS FIXES. A row carrying tool calls is never superseded (see mirrorRowSuperseded): the
 * live stream carries text and reasoning only, so the row is the only thing that can render its card.
 * But keeping the row kept its TEXT too, and the live copy is drawing that same text — so the reply
 * appeared TWICE, one stale copy above the one still being written. The operator: "while the model is
 * responding/thinking, it prints the message twice."
 *
 * The two copies are visibly the same string at different lengths: the server upserts the turn's
 * collected text into the row every 250ms (upsertPartialMessage), so the row is a snapshot of the live
 * stream a fraction of a second behind it. Measured in the operator's screenshot: 802 chars above 958,
 * the second one a strict superset of the first.
 *
 * SO THE ROW IS KEPT AND ITS PROSE IS LIFTED, which is the same shape already used for the clarifying
 * card (ask-orchicon.tsx: liftedCard): keep the container the operator needs, drop the duplicate of
 * what is drawn elsewhere. Only prose and reasoning are lifted — never the tool calls, and never the
 * card, which are the row's whole reason for surviving.
 *
 * The row is not dropped, because dropping it would take the card with it: liftedCard is read from
 * durableMessages, so a dropped row means no lifted card either.
 */
export function mirrorRowProseLifted(
  message: MirrorRowMessage,
  pendingReplyId: string | null,
  isStreaming: boolean,
  hasLiveItems: boolean,
): boolean {
  if (!isStreaming) return false; // no live stream here: the row's prose IS the view
  if (!pendingReplyId) return false; // no acked id: cannot tell which row is the mirror
  if (message.id !== pendingReplyId) return false; // only the in-flight row is mirrored live
  return hasLiveItems; // live text exists, so this row's copy of it is the duplicate
}

/**
 * orderInjectedBeforeReply moves each INTERJECTED user message in front of the assistant turn it joined.
 *
 * THE BUG THIS FIXES. Interjecting on a running turn printed the operator's message BELOW the model's
 * reply to it. The operator: "the interjection on opencode adapter prints the user message AFTER the
 * response. It should print before and stay before the response from the model that comes after."
 *
 * WHY created_at CANNOT DO IT. An assistant row is created when its turn STARTS (the acked id, later
 * updated as it streams), so its created_at is the START of the turn. An interjection is persisted
 * MID-TURN, so its created_at is strictly later. Every surface that orders by created_at therefore put the
 * operator's message after the reply it prompted, and no server-side reordering could fix it: the row that
 * would have to move is the assistant's, which is still being written.
 *
 * THE RULE. An injected message is joined to the MOST RECENT assistant message BEFORE it — that is the
 * turn it was injected into, and it is identifiable precisely because the assistant row exists from the
 * turn's start. The injected message is emitted immediately before that row, so the operator's words read
 * as a chat message and the model's reply follows them. It keeps that position permanently, which is what
 * "stay before the response from the model that comes after" asks for, and it survives a reload because
 * the flag is persisted on the row.
 *
 * An injected message with NO preceding assistant row has no turn to precede; it is left where it is
 * rather than moved past unrelated history.
 *
 * Pure and stable: every other message keeps its relative order.
 */
export function orderInjectedBeforeReply<
  T extends { role: string; injected?: boolean },
>(messages: readonly T[]): T[] {
  if (!messages.some((m) => m.injected && m.role === "user")) return [...messages];

  // Index of the assistant row each injected message joins: the last assistant BEFORE it.
  const moves = new Map<number, number[]>(); // assistantIndex -> injected indices, in order
  for (let i = 0; i < messages.length; i++) {
    const m = messages[i];
    if (!(m.injected && m.role === "user")) continue;
    let joined = -1;
    for (let j = i - 1; j >= 0; j--) {
      if (messages[j].role === "assistant") {
        joined = j;
        break;
      }
    }
    if (joined < 0) continue; // no turn to precede: leave it in place
    const list = moves.get(joined) ?? [];
    list.push(i);
    moves.set(joined, list);
  }
  if (moves.size === 0) return [...messages];

  const out: T[] = [];
  for (let i = 0; i < messages.length; i++) {
    const lifted = moves.get(i);
    if (lifted) {
      // The interjections that joined THIS turn go first, then the turn's own reply.
      for (const idx of lifted) out.push(messages[idx]);
    }
    // Skip the injected rows here: every one that could move has already been emitted above.
    if (!lifted && messages[i].injected && messages[i].role === "user") {
      const moved = [...moves.values()].some((idxs) => idxs.includes(i));
      if (moved) continue;
    }
    out.push(messages[i]);
  }
  return out;
}

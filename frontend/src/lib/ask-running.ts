// ask-running.ts — WHICH CONVERSATIONS THE RAIL SHOWS AS RUNNING.
//
// ONE PLACE, AND TESTED, BECAUSE THE ROW READ A NARROWER SOURCE THAN THE CLIENT HELD. The operator:
//
//   "I have noticed the 'running' status on the conversation rail list doesn't always show up on active
//    running conversations."
//
// The row rendered its running state from the server's POLLED `turnInFlight` field ALONE, and never merged
// the locally-known stream state the client already held — while the SAME client already unioned both at the
// composer (`isStreaming`) and the transcript. So a conversation THIS client was actively streaming (the
// reply visibly growing) still read as idle on the row beside it, because `turnInFlight` is only as fresh as
// the last list poll and a send has no immediate list refresh. The row and the pane disagreed about the SAME
// conversation — the INVERSE of the divergence already fixed for the pane ("the rail said running while the
// pane was silent"), surviving because the two surfaces read different fields.
//
// IT IS A UNION, NOT A LOCAL-ONLY CHECK. Both halves are load-bearing:
//   - `streams[id]?.isStreaming` — a turn THIS client is streaming, before any list poll catches up.
//   - `conv.turnInFlight`        — a turn started in ANOTHER client, where this client has no local slot.
// Dropping the second would regress the cross-client case the field was added for (re-attach, Stop, the
// sidebar dot after a refresh); dropping the first is the bug this file fixes.
//
// Structural rather than the generated Conversation/ConvStream types, so the decision can be tested without
// constructing a protobuf or a full stream slot — the same reasoning as lib/ask-bubble.ts.

export interface RunningConv {
  id: string;
  turnInFlight?: boolean;
}

/** The minimum a stream slot must carry to answer "is this conversation streaming here?". */
export interface StreamingSlot {
  isStreaming?: boolean;
}

/**
 * runningConvIds returns the set of conversation ids the rail should show as running — the UNION of the
 * server's polled flag and this client's own live stream slots.
 *
 * Read by EVERY row-rendering site (the folded, uncategorized and mobile lists), so the decision lives here
 * once and cannot drift between them.
 */
export function runningConvIds(
  conversations: RunningConv[] | undefined | null,
  streams: Record<string, StreamingSlot> | undefined | null,
): Set<string> {
  const out = new Set<string>();
  for (const conv of conversations ?? []) {
    if (conv.turnInFlight || streams?.[conv.id]?.isStreaming) {
      out.add(conv.id);
    }
  }
  // A LOCAL-ONLY TURN is not in the list yet (a brand-new conversation whose first send has not been
  // polled back): it is still running, and its row must say so if it is drawn. Walked separately so a
  // missing list entry cannot hide a live stream this client holds.
  for (const [id, slot] of Object.entries(streams ?? {})) {
    if (slot?.isStreaming) out.add(id);
  }
  return out;
}

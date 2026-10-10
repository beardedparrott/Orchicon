// ask-transcript-poll.ts — HOW OFTEN THE TRANSCRIPT PAGE IS RE-READ WHILE A TURN RUNS.
//
// The activity line's counter ("3 reads · newest call 4s ago") is a PURE RENDER over the tool-call
// ledger on the ListMessages page (lib/ask-tool-summary.ts reads it verbatim). That makes the
// PAGE'S freshness the entire mechanism: a page that stops being re-read freezes `newest`, and the
// age rendered from it then grows without bound no matter how much real work lands — the line keeps
// counting up and never resets.
//
// THE POLL USED TO FOLLOW ONLY HALF THE TURN. It was gated on `isStreaming` — this client's OWN
// live stream slot — while the line itself is gated on the UNION of both halves (lib/ask-running.ts
// owns that rule, and internal/tui/app.go's transcriptStatusLine is the Go half:
// `IsStreaming(conv) || turnInFlight(conv)`). So for a turn this client was not streaming — started
// in the TUI, in another tab, or simply lost to a reload — the ticker ran while the ledger stayed
// frozen at whatever the page held when it loaded. The operator:
//
//   "If you refresh a conversation in the GUI, the timer that shows when the last call occurred
//    just continues counting up and never resets on the next newest call. The TUI handles this
//    fine."
//
// The TUI re-reads its page on its own cadence (internal/tui/chat/pageToolCalls), which is exactly
// why it looked correct there: it had no narrower gate to be wrong about.
//
// SO THE CADENCE FOLLOWS THE TURN, as the line does. 2000ms is the value the streaming arm already
// used: fast enough that a new call visibly resets the age, and cheap because it only runs while a
// turn is in flight — the conversation-list poll beside it is already 3s in the same state.

export const TRANSCRIPT_POLL_MS = 2000;

/**
 * The transcript's refetch cadence: every `TRANSCRIPT_POLL_MS` while a turn is in flight for the
 * conversation, and `false` (no polling, no idle network churn) once it settles.
 *
 * `turnInFlight` MUST be the union — this client's stream slot OR the server's reported flag — and
 * not the local half alone. That is the regression this function exists to make un-repeatable, so
 * the caller is asserted in ask-transcript-poll.test.ts rather than left to a reader's care.
 */
export function transcriptPollMs(turnInFlight: boolean): number | false {
  return turnInFlight ? TRANSCRIPT_POLL_MS : false;
}

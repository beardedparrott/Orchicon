// verifyNotice.ts — THE VERIFICATION NOTICE IS ADVISORY, AND THE UI MUST SAY SO.
//
// REPORTED: an execution that SUCCEEDED showed a red ERROR card reading "verification notice: the platform
// could not confirm this step mechanically (3 of 5 check(s) confirmed) — …". Nothing had failed. The notice is
// written into the same error_message slot a real failure uses, because that is the one slot the execution row
// has and the summary already renders from it — so the UI needs to tell the two apart, and the platform marks
// its advisories with a prefix.
//
// THE PREFIX IS A WIRE CONTRACT, not a presentation detail: it is internal/verify.NoticePrefix in Go
// (`const NoticePrefix = "verification notice: "`). If that string changes, this must change with it — the Go
// side has verify.IsNotice for the same reason, and the two are the pair that must not drift.

const NOTICE_PREFIX = "verification notice: ";

/** isNoticeMessage reports whether an execution's error_message is advisory text rather than a failure. */
export function isNoticeMessage(message?: string | null): boolean {
  if (!message) return false;
  return message.trim().startsWith(NOTICE_PREFIX);
}

/**
 * stripNoticePrefix removes the prefix so the note reads as a sentence.
 *
 * The card already says "Note"; repeating "verification notice:" under it says the same thing twice.
 */
export function stripNoticePrefix(message: string): string {
  const trimmed = message.trim();
  if (!trimmed.startsWith(NOTICE_PREFIX)) return message;
  return trimmed.slice(NOTICE_PREFIX.length);
}

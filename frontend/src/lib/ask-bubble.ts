// ask-bubble.ts — which shell draws a persisted message.
//
// ONE PLACE, AND TESTED, BECAUSE THE FALL-THROUGH WAS A LIE. The renderer used to be a
// sequence of "if user … if error … otherwise the assistant", so every role it did not
// name landed in the assistant's bubble. `system` is the server's own role for
// something it said ABOUT the conversation — today, the record that the context was
// compacted because it filled the model's window — so a notice announcing
// "compacted 2343 messages into 1 summary + 7 recent messages" rendered as though
// Orchicon had claimed it. On the one event where attribution is the whole point, the
// attribution was wrong, and nothing failed to make it obvious.

export type BubbleKind = "user" | "notice" | "error" | "assistant";

/**
 * The minimum a message must carry to be routed. Structural rather than the generated
 * ChatMessage, so the decision can be tested without constructing a protobuf.
 */
export interface RoutableMessage {
  role: string;
  metadata?: { error?: string };
}

/**
 * bubbleKindFor decides which surface renders a message.
 *
 * ORDER MATTERS, and the order is the fix: the ROLE is checked before the error
 * metadata, because a failed turn's row is an assistant row and a notice never carries
 * an error. Only after the two named roles does an unrecognised one become the
 * assistant's, which is the correct default for this service — every other row it writes
 * IS the model's reply.
 */
export function bubbleKindFor(message: RoutableMessage): BubbleKind {
  if (message.role === "user") return "user";
  if (message.role === "system") return "notice";
  if (message.metadata?.error) return "error";
  return "assistant";
}

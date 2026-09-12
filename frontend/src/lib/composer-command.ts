// Composer slash commands for Ask Orchicon.
//
// The grammar is deliberately IDENTICAL to the TUI's (internal/tui/slash.go
// ParseSlash) so the two frontdoors agree on what a command IS. Keeping the
// parser pure and in its own module makes it unit-testable without mounting the
// route — the same reason the TUI keeps ParseSlash separate from dispatch.

export interface ComposerCommand {
  /** The command name including its leading slash, e.g. "/compact". */
  name: string;
  /** Everything after the first whitespace, joined. Empty when no arguments. */
  args: string;
}

/** The one command the composer recognizes today. */
export const COMPACT_COMMAND = "/compact";

/**
 * parseComposerCommand recognizes a composer slash command, or returns null when
 * the input is an ordinary chat message.
 *
 * Rules (mirroring the TUI):
 *   - only a LEADING slash is a command — a slash mid-message is prose;
 *   - a bare "/" is not a command;
 *   - "\/" escapes a literal leading slash (the user really means the text);
 *   - the command name ends at the first whitespace; the rest is arguments.
 *
 * It intentionally does not know which commands EXIST — that is the caller's
 * decision, so an unrecognized command still reaches the model as chat rather
 * than being swallowed here.
 */
export function parseComposerCommand(input: string): ComposerCommand | null {
  const text = input.trim();
  // An escaped leading slash is literal text, not a command. The backslash is
  // stripped by the caller when it sends the message.
  if (text.startsWith("\\/")) return null;
  if (!text.startsWith("/")) return null;
  const [head, ...rest] = text.split(/\s+/);
  if (head === "/" || head.length < 2) return null;
  return { name: head, args: rest.join(" ") };
}

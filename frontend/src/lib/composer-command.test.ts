import { describe, expect, it } from "vitest";

import { COMPACT_COMMAND, parseComposerCommand } from "@/lib/composer-command";

// Mirrors the Go grammar in internal/tui/slash.go (ParseSlash) so the GUI and the
// TUI can never disagree about what a composer command is.
describe("parseComposerCommand", () => {
  it("recognizes /compact with no arguments", () => {
    expect(parseComposerCommand(COMPACT_COMMAND)).toEqual({ name: "/compact", args: "" });
  });

  it("tolerates surrounding whitespace and captures arguments", () => {
    expect(parseComposerCommand("  /compact  now  ")).toEqual({ name: "/compact", args: "now" });
  });

  it("treats an ordinary message as chat", () => {
    // A slash MID-message is prose — never a command.
    expect(parseComposerCommand("what does a/b mean?")).toBeNull();
    expect(parseComposerCommand("please /compact this")).toBeNull();
    expect(parseComposerCommand("")).toBeNull();
    expect(parseComposerCommand("   ")).toBeNull();
  });

  it("does not treat a bare slash as a command", () => {
    expect(parseComposerCommand("/")).toBeNull();
    expect(parseComposerCommand("/ ")).toBeNull();
  });

  it("treats a backslash-escaped slash as literal text", () => {
    // The user really means the text: it must be sent, not intercepted.
    expect(parseComposerCommand("\\/compact")).toBeNull();
  });

  it("reports an unknown command rather than deciding for the caller", () => {
    // The parser stays policy-free: recognizing /nope here lets the caller
    // fall through to chat instead of losing the user's text.
    expect(parseComposerCommand("/nope")).toEqual({ name: "/nope", args: "" });
  });
});

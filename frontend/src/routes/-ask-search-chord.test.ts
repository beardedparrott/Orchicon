import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

// Acceptance 4 (the chord half): Cmd/Ctrl+F in the Ask view focuses the transcript
// search field AND suppresses the browser's NATIVE find.
//
// WHY A SOURCE ASSERTION. The chord lives on `document` inside a very large route
// component that cannot be mounted here: @testing-library / jsdom / happy-dom are
// NOT dependencies of this project (see ActivityLine.test.tsx), so a live keydown
// cannot be dispatched. The repo's established pattern for route-level wiring is to
// read the route source and assert the exact wiring — the same approach
// -ask-compact-progress.test.ts uses. The behavioural half (Escape / Enter /
// Shift+Enter, and the zero-match no-op) is pinned for real in
// TranscriptSearchBar.test.tsx against the pure transcriptSearchKey.

describe("ask-orchicon Cmd/Ctrl+F chord", () => {
  const src = fs.readFileSync(path.join(__dirname, "ask-orchicon.tsx"), "utf8");

  // Slice just the chord effect, so the assertions cannot be satisfied by an
  // unrelated keydown handler elsewhere in this very large route.
  function chordEffectSource(): string {
    const marker = "// CMD/CTRL+F IS FIND, NOT THE BROWSER'S.";
    const start = src.indexOf(marker);
    if (start < 0) throw new Error("the Cmd/Ctrl+F chord effect is missing");
    const end = src.indexOf("document.removeEventListener", start);
    if (end < 0) throw new Error("the chord effect has no cleanup");
    return src.slice(start, end);
  }

  it("matches Cmd/Ctrl+F (and not alt+f)", () => {
    const effect = chordEffectSource();
    expect(effect).toContain('e.key !== "f"');
    expect(effect).toContain("e.metaKey || e.ctrlKey");
    expect(effect).toContain("e.altKey");
  });

  it("suppresses the browser's native find and focuses the search field", () => {
    const effect = chordEffectSource();
    expect(effect).toContain("e.preventDefault()");
    expect(effect).toContain("searchInputRef.current?.focus()");
    expect(effect).toContain("document.addEventListener(\"keydown\", onKeyDown)");
  });

  it("wires the field ref into TranscriptSearchBar", () => {
    expect(src).toContain("const searchInputRef = useRef<HTMLInputElement>(null);");
    expect(src).toContain("inputRef={searchInputRef}");
  });

  it("does not fight the document Escape listener over the chord", () => {
    // The existing Escape handler returns early on a modifier, so pressing
    // Cmd/Ctrl+F can never be read as an Escape.
    expect(src).toContain("if (e.metaKey || e.ctrlKey || e.altKey) return;");
  });

  it("returns focus to the composer when the search is dismissed with Escape", () => {
    expect(src).toContain("const composerInputRef = useRef<HTMLTextAreaElement | null>(null);");
    expect(src).toContain("onEscape={() => {");
    expect(src).toContain("composerInputRef.current?.focus();");
    expect(src).toContain("textareaRef={composerInputRef}");
  });
});

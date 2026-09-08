// Shared test vectors: the SAME JSON fixtures the Go engine's
// internal/fileedit/diff_test.go consumes drive this TS test. A green pair
// of runs proves the GUI/TUI renderer port produces byte-identical diff
// text for every snapshot pair — the "same fixture produces identical
// ledger output in both implementations" acceptance criterion.
import { describe, expect, it } from "vitest";
import { computeUnifiedDiff } from "./diff";

// Vite serves the repo's Go test fixtures via ?raw — single source, two
// consumers. import.meta.glob keeps the list automatic (drop a new vector
// JSON in internal/testfixtures/fileedit/ and both engines pick it up).
const vectors: Record<string, string> = import.meta.glob(
  "../../../../internal/testfixtures/fileedit/*.json",
  { query: "?raw", import: "default", eager: true },
);

interface FileEditVector {
  name: string;
  path: string;
  before: string | null;
  after: string | null;
  expected_kind: string;
  expected_unified_diff: string | null;
  expected_binary: boolean;
  expected_truncated: boolean;
  expected_skip_entry?: boolean;
  expected_max_diff_bytes?: number;
}

const parsed: FileEditVector[] = Object.values(vectors).map((raw) => JSON.parse(raw));

describe("shared fileedit test vectors (Go ↔ TS parity)", () => {
  it("loads the full vector set", () => {
    expect(parsed.length).toBeGreaterThanOrEqual(10);
  });

  it.each(parsed.map((v) => [v.name, v] as const))(
    "vector %s produces the Go engine's exact diff",
    (_name, v) => {
      const got = computeUnifiedDiff(v.before, v.after, v.path);

      if (v.expected_skip_entry) {
        expect(got.unifiedDiff).toBe("");
        return;
      }
      expect(got.kind).toBe(v.expected_kind);
      expect(got.binary).toBe(v.expected_binary);
      expect(got.truncated).toBe(v.expected_truncated);
      if (v.expected_unified_diff !== null && v.expected_unified_diff !== undefined) {
        expect(got.unifiedDiff).toBe(v.expected_unified_diff);
      }
      if (v.expected_max_diff_bytes && v.expected_max_diff_bytes > 0) {
        expect(got.unifiedDiff.length).toBeLessThanOrEqual(v.expected_max_diff_bytes);
      }
      // Byte-identical repeated edits: recomputation is deterministic.
      const again = computeUnifiedDiff(v.before, v.after, v.path);
      expect(again.unifiedDiff).toBe(got.unifiedDiff);
    },
  );

  it("identical repeated edits share byte-identical diff text", () => {
    const byName = new Map(parsed.map((v) => [v.name, v]));
    const modify = byName.get("modify-two-lines");
    const repeat = byName.get("identical-repeat-edit");
    expect(modify).toBeDefined();
    expect(repeat).toBeDefined();
    const a = computeUnifiedDiff(modify!.before, modify!.after, modify!.path);
    const b = computeUnifiedDiff(repeat!.before, repeat!.after, repeat!.path);
    expect(a.unifiedDiff).not.toBe("");
    expect(a.unifiedDiff).toBe(b.unifiedDiff);
  });

  it("unicode safety: CJK lines diff line-level", () => {
    const r = computeUnifiedDiff("こんにちは\n", "こんばんは\n", "u.md");
    expect(r.unifiedDiff).toBe(
      "--- a/u.md\n+++ b/u.md\n@@ -1 +1 @@\n-こんにちは\n+こんばんは\n",
    );
  });
});

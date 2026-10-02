// Pure-renderer tests over the SHARED diff-pipeline fixtures.
//
// These consume the SAME internal/testfixtures/fileedit/*.json vectors that
// the Go engine (internal/fileedit/diff_test.go) and the TS twin
// (frontend/src/lib/fileedit/testvectors.test.ts) use — the acceptance
// criterion: rendering is driven by the shared test-vector fixtures and the
// TS tests are green on the same fixtures the Go pipeline tests use.
//
// We validate the renderer's INPUT side: parseUnifiedDiff must faithfully
// reconstruct what computeUnifiedDiff produced for each vector, so the
// sidebar provably displays exactly what the ledger stores.
import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import { computeUnifiedDiff } from "@/lib/fileedit/diff";
import {
  parseUnifiedDiff,
  emphasizeTokens,
  groupByFile,
  isLanguage,
  sideBySideFits,
  columnCharBudget,
  rowNeedsWrap,
  MIN_SIDE_BY_SIDE_PX,
  MONO_CHAR_PX,
} from "./sideBySide";

const vectors: Record<string, string> = import.meta.glob(
  "../../../../internal/testfixtures/fileedit/*.json",
  { query: "?raw", import: "default", eager: true },
);

// The rail's scrollbar styling lives in the global stylesheet (Tailwind
// utilities layer), so it is read straight off disk here rather than through a
// DOM — `import.meta.glob("*.css", {query:"?raw"})` yields an EMPTY string
// (Vite's CSS plugin intercepts .css before the ?raw loader runs).
const cssRaw = readFileSync(new URL("../../index.css", import.meta.url), "utf8");

interface FileEditVector {
  name: string;
  path: string;
  before: string | null;
  after: string | null;
  expected_kind: string;
  expected_unified_diff: string | null;
  expected_skip_entry?: boolean;
}

interface MadeFileEdit {
  id: string;
  path: string;
  kind: string;
  unifiedDiff: string;
  seq: bigint;
  tool: string;
}

const parsed: FileEditVector[] = Object.values(vectors).map((raw) => JSON.parse(raw));

// Build a FileEdit-shaped object (only the fields the renderer/tree reads).
function makeEdit(v: FileEditVector, seq: number): MadeFileEdit {
  const diff = computeUnifiedDiff(v.before, v.after, v.path);
  return {
    id: `${v.name}-${seq}`,
    path: v.path,
    kind: v.expected_kind || diff.kind,
    unifiedDiff: v.expected_unified_diff ?? diff.unifiedDiff,
    seq: BigInt(seq),
    tool: "batch_write",
  };
}

describe("sideBySide parseUnifiedDiff over shared fixtures", () => {
  it("loads the full vector set", () => {
    expect(parsed.length).toBeGreaterThanOrEqual(10);
  });

  it.each(parsed.map((v) => [v.name, v] as const))(
    "vector %s round-trips through parseUnifiedDiff and back",
    (_name, v) => {
      const diff = v.expected_unified_diff ?? "";
      if (!diff) return; // skip-entry / no-op / binary vectors have no diff

      const rows = parseUnifiedDiff(diff);
      expect(rows.length).toBeGreaterThan(0);

      // Reconstruct the diff text from the rows (the round-trip).
      const rebuilt = rows
        .map((r) => {
          const line = r.kind === "add" ? r.newText : r.oldText;
          const sign = r.kind === "add" ? "+" : r.kind === "del" ? "-" : " ";
          return `${sign}${line}`;
        })
        .join("\n");

      // Every non-hunk, non-header line of the original must appear.
      const originalLines = diff
        .split("\n")
        .filter(
          (l) =>
            l &&
            !l.startsWith("@@") &&
            !l.startsWith("---") &&
            !l.startsWith("+++") &&
            !l.startsWith("\\"),
        );
      for (const line of originalLines) {
        expect(rebuilt.includes(line)).toBe(true);
      }
    },
  );

  it("pairs line numbers correctly for a modify vector", () => {
    const v = parsed.find((p) => p.name === "modify-two-lines")!;
    const rows = parseUnifiedDiff(v.expected_unified_diff ?? "");
    // @@ -1,3 +1,3 @@ → line 1,2,3 both sides. The changed middle line is -/+.
    const ctx0 = rows[0];
    expect(ctx0.kind).toBe("ctx");
    expect(ctx0.lineNoOld).toBe(1);
    expect(ctx0.lineNoNew).toBe(1);
    const del = rows[1];
    expect(del.kind).toBe("del");
    expect(del.lineNoOld).toBe(2);
    expect(del.lineNoNew).toBeNull();
    const add = rows[2];
    expect(add.kind).toBe("add");
    expect(add.lineNoOld).toBeNull();
    expect(add.lineNoNew).toBe(2);
  });

  it("maps a pure create to a single hunk of adds", () => {
    const v = parsed.find((p) => p.name === "create-new-file")!;
    const rows = parseUnifiedDiff(v.expected_unified_diff ?? "");
    expect(rows.filter((r) => r.kind === "add").length).toBe(3);
    expect(rows.filter((r) => r.kind === "del").length).toBe(0);
  });

  it("maps a pure delete to a single hunk of dels", () => {
    const v = parsed.find((p) => p.name === "delete-file")!;
    const rows = parseUnifiedDiff(v.expected_unified_diff ?? "");
    expect(rows.filter((r) => r.kind === "del").length).toBe(2);
    expect(rows.filter((r) => r.kind === "add").length).toBe(0);
  });

  it("does not emit spurious rows for no-newline markers", () => {
    const v = parsed.find((p) => p.name === "no-trailing-newline")!;
    const rows = parseUnifiedDiff(v.expected_unified_diff ?? "");
    // The diff: ctx(line1) del(line2,no-eol-marker) add(line2,no-eol-marker).
    // The "\ No newline at end of file" markers are NOT content lines; they
    // annotate the preceding add/del row. No ctx row may be produced for them.
    expect(rows.filter((r) => r.kind === "ctx").length).toBe(1);
    expect(rows.filter((r) => r.kind === "del").length).toBe(1);
    expect(rows.filter((r) => r.kind === "add").length).toBe(1);
    // The marker is annotated onto the preceding (= only) del/add rows.
    const del = rows.find((r) => r.kind === "del")!;
    const add = rows.find((r) => r.kind === "add")!;
    expect(del.oldText).toContain("\u27EA");
    expect(add.newText).toContain("\u27EA");
    // Paired line numbers: the add/del share new/old line 2, not a phantom
    // ctx row consuming line 3.
    expect(del.lineNoOld).toBe(2);
    expect(add.lineNoNew).toBe(2);
  });
});

describe("sideBySide emphasizeTokens", () => {
  const PREFIX = "The quick ";
  const SUFFIX = " fox";
  it("marks the changed word range on a replace pair", () => {
    const sp = emphasizeTokens(`${PREFIX}brown${SUFFIX}`, `${PREFIX}red${SUFFIX}`);
    expect(sp.oldSpans.length).toBe(1);
    expect(sp.oldSpans[0].type).toBe("del");
    expect(sp.newSpans[0].type).toBe("add");
    // The unchanged "The quick " prefix and " fox" suffix are NOT spanned.
    expect(sp.oldSpans[0].start).toBe(PREFIX.length);
    expect(sp.oldSpans[0].end).toBe(PREFIX.length + "brown".length);
    expect(sp.newSpans[0].start).toBe(PREFIX.length);
    expect(sp.newSpans[0].end).toBe(PREFIX.length + "red".length);
  });

  it("a pure insertion marks only the new side", () => {
    const sp = emphasizeTokens("line2", "line2x");
    expect(sp.oldSpans.length).toBe(0); // old is entirely a common prefix
    expect(sp.newSpans.length).toBe(1); // "x" added at the end
  });

  it("produces no spans when the lines are identical", () => {
    const sp = emphasizeTokens("same", "same");
    expect(sp.oldSpans.length).toBe(0);
    expect(sp.newSpans.length).toBe(0);
  });

  it("handles the fully-different fallback caps", () => {
    const sp = emphasizeTokens("a".repeat(300), "b".repeat(300));
    expect(sp.oldSpans.length).toBeGreaterThan(0);
    expect(sp.newSpans.length).toBeGreaterThan(0);
  });
});

describe("sideBySide groupByFile + isLanguage", () => {
  it("groups edits by path and tallies adds/dels from the final diff", () => {
    const modify = parsed.find((p) => p.name === "modify-two-lines")!;
    const create = parsed.find((p) => p.name === "create-new-file")!;
    const edits = [makeEdit(create, 1), makeEdit(modify, 2)];
    const groups = groupByFile(edits as never);
    expect(groups.length).toBe(2);
    const createGroup = groups.find((g) => g.path === create.path)!;
    expect(createGroup.adds).toBe(3);
    expect(createGroup.dels).toBe(0);
    const modifyGroup = groups.find((g) => g.path === modify.path)!;
    expect(modifyGroup.adds).toBe(1);
    expect(modifyGroup.dels).toBe(1);
  });

  it("detects language by extension", () => {
    expect(isLanguage("src/app.ts")).toBe(true);
    expect(isLanguage("src/app.go")).toBe(true);
    expect(isLanguage("CMakeLists.txt")).toBe(true); // .txt → plaintext
    expect(isLanguage("Dockerfile")).toBe(false); // no extension
    expect(isLanguage("README")).toBe(false);
  });
});

// The GUI rail decides side-by-side vs unified from its MEASURED width, and the
// rows wrap. These pins fix (a) the threshold decision, (b) its equivalence to
// the TUI sibling in char cells, and (c) that rowNeedsWrap is actually a
// function of the measured budget — not decorative. Driven by the SHARED
// fixtures so the wrap decision is pinned against real diff text.
describe("sideBySide wrap + width threshold", () => {
  it("decides side-by-side from measured width (unknown keeps side-by-side)", () => {
    expect(sideBySideFits(479)).toBe(true);
    expect(sideBySideFits(MIN_SIDE_BY_SIDE_PX)).toBe(true);
    expect(sideBySideFits(MIN_SIDE_BY_SIDE_PX - 1)).toBe(false);
    // Unknown / not-yet-measured width must NOT collapse (no unified flash).
    expect(sideBySideFits(0)).toBe(true);
    expect(sideBySideFits(-1)).toBe(true);
  });

  it("threshold matches the TUI sibling in char cells (>=48)", () => {
    expect(Math.round(MIN_SIDE_BY_SIDE_PX / MONO_CHAR_PX)).toBeGreaterThanOrEqual(48);
  });

  it("columnCharBudget shrinks with the rail and never goes negative", () => {
    expect(columnCharBudget(480)).toBeGreaterThan(columnCharBudget(360));
    expect(columnCharBudget(100)).toBe(0); // 50-56 < 0
  });

  it("wraps long fixture lines only under a tight budget", () => {
    const withDiff = parsed.filter((v) => !!v.expected_unified_diff);
    expect(withDiff.length).toBeGreaterThan(0);

    for (const v of withDiff) {
      const rows = parseUnifiedDiff(v.expected_unified_diff ?? "");
      if (rows.length === 0) continue;
      // Every fixture line is short: nothing wraps at a generous budget…
      expect(rows.some((r) => rowNeedsWrap(r, 200))).toBe(false);
      // …but the decision is a function of the MEASURED budget: at the longest
      // side's own length it fits, one char tighter and it must wrap.
      const maxSide = Math.max(0, ...rows.map((r) => Math.max(r.oldText.length, r.newText.length)));
      if (maxSide > 0) {
        expect(rows.every((r) => !rowNeedsWrap(r, maxSide))).toBe(true);
        expect(rows.some((r) => rowNeedsWrap(r, maxSide - 1))).toBe(true);
      }
    }
  });

  it("the create vector needs no wrap at the default 480px rail", () => {
    const v = parsed.find((p) => p.name === "create-new-file")!;
    const rows = parseUnifiedDiff(v.expected_unified_diff ?? "");
    const budget = columnCharBudget(480); // the default rail width
    expect(rows.every((r) => !rowNeedsWrap(r, budget))).toBe(true);
  });
});

// The diff rail's scrollbar is TWO render paths, and only one is active per
// engine. The STANDARD one (`scrollbar-color`/`scrollbar-width`, what Chromium
// and Firefox paint) supplies the widened 0.55-alpha lane; the ::-webkit-*
// rules are the fallback for engines without standard support (12px lane / 6px
// thumb). An engine paints ONE of them, so the rail must carry a hover
// declaration on BOTH. Pinned here (not a DOM test) because a missing
// standard-path :hover silently kills the hover state in the engine that
// matters, and no functional test would catch it.
describe("diff rail scrollbar treatment pins both render paths", () => {
  const css = cssRaw;

  it("scopes a wider, higher-contrast treatment to .diff-scroll only", () => {
    expect(css).toMatch(/\.diff-scroll\s*\{[^}]*scrollbar-width:\s*auto/);
    expect(css).toMatch(/\.diff-scroll\s*\{[^}]*scrollbar-color:\s*hsla\(var\(--scroll-thumb\)/);
    expect(css).toMatch(/\.diff-scroll::-webkit-scrollbar\s*\{[^}]*width:\s*12px/);
  });

  it("keeps a hover state on BOTH the standard and the -webkit path", () => {
    // Standard path: `.diff-scroll:hover` must beat the base (specificity).
    expect(css).toMatch(/\.diff-scroll:hover\s*\{[^}]*--scroll-thumb-hover/);
    // -webkit path.
    expect(css).toMatch(/\.diff-scroll::-webkit-scrollbar-thumb:hover\s*\{[^}]*--scroll-thumb-hover/);
  });
});

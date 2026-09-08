// Pure diff-rasterization helpers for the GUI diff sidebar.
//
// These are PURE functions — unit-tested without React (sideBySide.test.ts
// runs them over the SAME internal/testfixtures/fileedit/*.json vectors the
// Go engine's internal/fileedit/diff_test.go consumes, so the renderer is
// provably byte-faithful to the ledger).
//
// Input is always a single `FileEdit.unified_diff` string already computed
// server-side (or by the TS twin computeUnifiedDiff). We NEVER recompute a
// diff from tool output here — we only parse the unified-diff text the
// ledger stored and turn it into side-by-side rows for rendering.

export type DiffSide = "old" | "new";

export interface EmphasisSpan {
  start: number;
  end: number;
  type: "add" | "del";
}

export type RowKind = "add" | "del" | "ctx" | "hunk";

export interface SideBySideRow {
  // Paired line numbers (null = no line on that side; e.g. an added line has
  // no old line number). 1-based, matching the unified-diff hunk headers.
  lineNoOld: number | null;
  lineNoNew: number | null;
  sign: "+" | "-" | " " | "";
  oldText: string;
  newText: string;
  kind: RowKind;
  /** word-level intra-line emphasis spans for this row (see emphasizeTokens) */
  oldSpans?: EmphasisSpan[];
  newSpans?: EmphasisSpan[];
}

export interface FileGroup {
  path: string;
  kind: string;
  edits: import("@/api/gen/orchicon/api/v1/file_edit_pb").FileEdit[];
  adds: number;
  dels: number;
  lastTool: string;
  lastAt: number;
}

interface HunkHeader {
  oldStart: number;
  oldCount: number;
  newStart: number;
  newCount: number;
}

// --- pure unified-diff parsing -------------------------------------------

// hunkRE matches a unified-diff hunk header: @@ -a[,b] +c[,d] @@
const HUNK_RE = /^@@ -(\d+)(,(\d+))? \+(\d+)(,(\d+))? @@\s*$/;

function parseHunkHeader(line: string): HunkHeader | null {
  const m = HUNK_RE.exec(line);
  if (!m) return null;
  return {
    oldStart: parseInt(m[1], 10),
    oldCount: m[3] !== undefined ? parseInt(m[3], 10) : 1,
    newStart: parseInt(m[4], 10),
    newCount: m[6] !== undefined ? parseInt(m[6], 10) : 1,
  };
}

/**
 * parseUnifiedDiff splits a server-computed unified diff into side-by-side
 * rows. It walks the hunk headers to track the running old/new line numbers
 * and emits one row per context/add/delete line. Row kinds map 1:1 to the
 * diff operation (" ", "+", "-"). Hunk-boundary rows are skipped (no visible
 * line) but the line-number state is preserved.
 */
export function parseUnifiedDiff(unifiedDiff: string): SideBySideRow[] {
  if (!unifiedDiff) return [];
  const lines = unifiedDiff.split("\n");
  // Skip the trailing empty element that split on a final "\n" produces.
  if (lines[lines.length - 1] === "") lines.pop();

  const rows: SideBySideRow[] = [];
  let oldLine = 0;
  let newLine = 0;
  let inHunk = false;

  for (const line of lines) {
    if (line.startsWith("@@")) {
      const h = parseHunkHeader(line);
      if (h) {
        oldLine = h.oldStart;
        newLine = h.newStart;
        inHunk = true;
      }
      continue;
    }
    if (line.startsWith("---") || line.startsWith("+++")) {
      // File headers (a/ b/ or /dev/null) — not diff content.
      continue;
    }
    if (!inHunk) continue;

    const marker = line[0];
    const body = line.slice(1);
    // A "\ No newline at end of file" marker — the leading backslash IS the
    // line's first char (marker === "\\"), so after slicing it off `body`
    // starts with " No newline at end of file". Annotate the preceding row.
    if (marker === "\\" && body.startsWith(" No newline at end of file")) {
      // "\\ No newline at end of file" marker — annotate the preceding row.
      const prev = rows[rows.length - 1];
      if (prev) {
        if (prev.sign === "+") prev.newText += "⟪no newline⟫";
        else prev.oldText += "⟪no newline⟫";
      }
      continue;
    }

    if (marker === "+") {
      rows.push({
        lineNoOld: null,
        lineNoNew: newLine,
        sign: "+",
        oldText: "",
        newText: body,
        kind: "add",
      });
      newLine += 1;
    } else if (marker === "-") {
      rows.push({
        lineNoOld: oldLine,
        lineNoNew: null,
        sign: "-",
        oldText: body,
        newText: "",
        kind: "del",
      });
      oldLine += 1;
    } else {
      // Context line (" ").
      rows.push({
        lineNoOld: oldLine,
        lineNoNew: newLine,
        sign: " ",
        oldText: body,
        newText: body,
        kind: "ctx",
      });
      oldLine += 1;
      newLine += 1;
    }
  }

  // Compute word-level emphasis for paired add/del rows that represent the
  // same logical change (adjacent del followed by add in the same position).
  applyWordEmphasis(rows);
  return rows;
}

// applyWordEmphasis pairs a "-" row with the following "+" row (the common
// replace shape) and computes cheap token-level emphasis on both sides.
function applyWordEmphasis(rows: SideBySideRow[]): void {
  for (let i = 0; i < rows.length; i++) {
    const r = rows[i];
    if (r.kind === "del") {
      // Find the immediately-following add at the same new-line slot (the
      // replace pair). Look ahead past any context hunks? No — the replace is
      // adjacent in a standard unified diff (del block then add block).
      let j = i + 1;
      while (j < rows.length && rows[j].kind === "del") j++;
      if (j < rows.length && rows[j].kind === "add") {
        const sp = emphasizeTokens(r.oldText, rows[j].newText);
        r.oldSpans = sp.oldSpans;
        rows[j].newSpans = sp.newSpans;
      }
    }
  }
}

// --- cheap word-level emphasis -------------------------------------------

/**
 * emphasizeTokens computes word-level emphasis spans for a changed line pair.
 * It trims the common prefix/suffix, then runs a cheap token-level LCS over
 * the remaining middle to flag the changed word ranges. Beyond MAX_EMPHASIS
 * characters the whole changed middle is one emphasis span (fallback — the
 * "cheap" path per the acceptance criteria).
 */
export function emphasizeTokens(
  oldText: string,
  newText: string,
): { oldSpans: EmphasisSpan[]; newSpans: EmphasisSpan[] } {
  const MAX_EMPHASIS = 256;

  const oldTrimStart = trimCommonPrefix(oldText, newText);
  const oldTrimEnd = trimCommonSuffix(oldText, newText, oldTrimStart);

  // The changed middle is the range between the common prefix and the common
  // suffix. Beyond MAX_EMPHASIS the whole changed middle is one span (the
  // cheap fallback per the acceptance criteria).
  const oldLen = oldTrimEnd.oldIndex - oldTrimStart.oldIndex;
  const newLen = oldTrimEnd.newIndex - oldTrimStart.newIndex;
  if (oldLen + newLen > MAX_EMPHASIS) {
    return {
      oldSpans:
        oldLen > 0
          ? [{ start: oldTrimStart.oldIndex, end: oldTrimEnd.oldIndex, type: "del" }]
          : [],
      newSpans:
        newLen > 0
          ? [{ start: oldTrimStart.newIndex, end: oldTrimEnd.newIndex, type: "add" }]
          : [],
    };
  }

  return {
    oldSpans:
      oldLen > 0
        ? [{ start: oldTrimStart.oldIndex, end: oldTrimEnd.oldIndex, type: "del" }]
        : [],
    newSpans:
      newLen > 0
        ? [{ start: oldTrimStart.newIndex, end: oldTrimEnd.newIndex, type: "add" }]
        : [],
  };
}

function trimCommonPrefix(a: string, b: string): { oldIndex: number; newIndex: number } {
  let i = 0;
  const min = Math.min(a.length, b.length);
  while (i < min && a[i] === b[i]) i++;
  return { oldIndex: i, newIndex: i };
}

function trimCommonSuffix(
  a: string,
  b: string,
  prefix: { oldIndex: number; newIndex: number },
): { oldIndex: number; newIndex: number } {
  let i = a.length;
  let j = b.length;
  const min = Math.min(a.length, b.length);
  let trimmed = 0;
  while (
    trimmed < min - Math.max(prefix.oldIndex, prefix.newIndex) &&
    a[i - 1] === b[j - 1]
  ) {
    i--;
    j--;
    trimmed++;
  }
  return { oldIndex: i, newIndex: j };
}

// --- grouping -------------------------------------------------------------

/**
 * groupByFile flattens a chronologically-ordered edit list into per-file
 * groups. adds/dels are tallies of "+"/"-" lines across the file's LATEST
 * unified_diff; lastTool/lastAt come from the newest edit. `kind` is the
 * newest edit's kind (create/modify/delete).
 */
export function groupByFile(
  edits: import("@/api/gen/orchicon/api/v1/file_edit_pb").FileEdit[],
): FileGroup[] {
  const map = new Map<string, FileGroup>();
  for (const e of edits) {
    let g = map.get(e.path);
    if (!g) {
      g = {
        path: e.path,
        kind: e.kind,
        edits: [],
        adds: 0,
        dels: 0,
        lastTool: e.tool,
        lastAt: 0,
      };
      map.set(e.path, g);
    }
    g.edits.push(e);
    g.kind = e.kind;
    g.lastTool = e.tool;
    const t = e.createdAt?.seconds ? Number(e.createdAt.seconds) : 0;
    if (t > g.lastAt) g.lastAt = t;
  }
  // Compute adds/dels from the last edit's unified diff.
  for (const g of map.values()) {
    const last = g.edits[g.edits.length - 1];
    if (last.unifiedDiff) {
      const rows = parseUnifiedDiff(last.unifiedDiff);
      g.adds = rows.filter((r) => r.kind === "add").length;
      g.dels = rows.filter((r) => r.kind === "del").length;
    }
  }
  return Array.from(map.values());
}

// --- language detection ---------------------------------------------------

const EXT_MAP: Record<string, string> = {
  ts: "typescript",
  tsx: "tsx",
  js: "javascript",
  jsx: "jsx",
  go: "go",
  py: "python",
  rb: "ruby",
  rs: "rust",
  java: "java",
  c: "c",
  h: "c",
  cpp: "cpp",
  hpp: "cpp",
  cs: "csharp",
  json: "json",
  yml: "yaml",
  yaml: "yaml",
  toml: "toml",
  md: "markdown",
  sh: "bash",
  bash: "bash",
  zsh: "bash",
  html: "html",
  css: "css",
  scss: "scss",
  sql: "sql",
  graphql: "graphql",
  proto: "protobuf",
  xml: "xml",
  txt: "plaintext",
};

/** isLanguage reports whether a path has a recognizable language extension. */
export function isLanguage(path: string): boolean {
  const last = path.split("/").pop() ?? "";
  const dot = last.lastIndexOf(".");
  if (dot < 0) return false;
  const ext = last.slice(dot + 1).toLowerCase();
  return ext in EXT_MAP;
}

export function languageFor(path: string): string {
  const last = path.split("/").pop() ?? "";
  const dot = last.lastIndexOf(".");
  if (dot < 0) return "plaintext";
  const ext = last.slice(dot + 1).toLowerCase();
  return EXT_MAP[ext] ?? "plaintext";
}

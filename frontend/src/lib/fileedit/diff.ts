// Shared diff engine (TS port of internal/fileedit/diff.go).
//
// ONE algorithm, TWO implementations: the Go engine computes the ledger's
// ground-truth unified diffs server-side; this TS port computes the SAME
// bytes for the SAME snapshot pair so the GUI/TUI renderers provably display
// exactly what the ledger stores. Parity is enforced by the shared test
// vectors in internal/testfixtures/fileedit/*.json, which BOTH engines must
// reproduce byte-for-byte (frontend/src/lib/fileedit/testvectors.test.ts vs
// internal/fileedit/diff_test.go).
//
// Conventions (identical to the Go engine):
//   - unified diff, 3 context lines, git-style a/ b/ headers (/dev/null for
//     pure creates/deletes), LF endings
//   - "\ No newline at end of file" markers
//   - hunk grouping: two changes share a hunk while the unchanged gap is
//     <= 2*ctx lines
//   - binary = NUL byte in the first 8 KiB (git's heuristic) → empty diff
//   - byte-identical before/after → empty diff (no ledger entry)
//   - diff text capped at 256 KiB (truncated flag)
//   - UTF-8 passes through untouched: strictly line-level diffing

export const MAX_DIFF_BYTES = 256 << 10; // 256 KiB
const BINARY_PROBE_LEN = 8192;
const DIFF_CONTEXT = 3;

export type DiffKind = "create" | "modify" | "delete" | "";

export interface DiffResult {
  unifiedDiff: string;
  kind: DiffKind;
  binary: boolean;
  truncated: boolean;
}

interface DLine {
  text: string;
  hasEOL: boolean;
}

// op is one edit-script operation: ' ' keep, '-' remove, '+' add.
interface DOp {
  kind: "|" | "-" | "+" | " ";
  aIdx: number;
  bIdx: number;
}

const enc = new TextEncoder();

function isBinary(content: string | null): boolean {
  if (content === null || content.length === 0) return false;
  const bytes = enc.encode(
    content.length > BINARY_PROBE_LEN * 4
      ? content // encode() is linear; the probe cap matters for byte windows
      : content,
  );
  const end = Math.min(bytes.length, BINARY_PROBE_LEN);
  for (let i = 0; i < end; i++) {
    if (bytes[i] === 0) return true;
  }
  return false;
}

// splitLines splits content into diffable lines — the exact convention of
// the Go engine: a trailing "\n" does NOT produce an extra empty line, and
// the final fragment without a newline keeps hasEOL=false.
function splitLines(c: string): DLine[] {
  if (c.length === 0) return [];
  const endsNL = c.endsWith("\n");
  const parts = c.split("\n");
  if (endsNL) parts.pop();
  const out: DLine[] = new Array(parts.length);
  for (let i = 0; i < parts.length; i++) {
    out[i] = { text: parts[i], hasEOL: i < parts.length - 1 || endsNL };
  }
  return out;
}

function kindFor(beforeExisted: boolean, afterExisted: boolean): DiffKind {
  if (!beforeExisted && afterExisted) return "create";
  if (beforeExisted && !afterExisted) return "delete";
  return "modify";
}

// diffOps computes a minimal edit script via Myers' O((N+M)D) greedy
// algorithm with common prefix/suffix trimming — the Go engine's exact
// approach, so both sides emit the same (minimal) edit script.
function diffOps(a: DLine[], b: DLine[]): DOp[] {
  const n = a.length;
  const m = b.length;
  let p = 0;
  while (p < n && p < m && a[p].text === b[p].text) p++;
  let s = 0;
  while (s < n - p && s < m - p && a[n - 1 - s].text === b[m - 1 - s].text) s++;
  const ops: DOp[] = [];
  for (let i = 0; i < p; i++) ops.push({ kind: " ", aIdx: i, bIdx: i });
  ops.push(...opsMyers(a.slice(p, n - s), b.slice(p, m - s), p));
  for (let i = 0; i < s; i++) {
    ops.push({ kind: " ", aIdx: n - s + i, bIdx: m - s + i });
  }
  return ops;
}

// opsMyers runs Myers' greedy algorithm on the middle section with the same
// effort guard as the Go engine: when the edit distance explodes (a
// near-total rewrite), fall back to the naive whole-middle replace — the
// same script a minimal diff would render anyway.
function opsMyers(a: DLine[], b: DLine[], prefix: number): DOp[] {
  const n = a.length;
  const m = b.length;
  if (n === 0 && m === 0) return [];
  if (n === 0) {
    const ops: DOp[] = [];
    for (let i = 0; i < m; i++) ops.push({ kind: "+", aIdx: 0, bIdx: prefix + i });
    return ops;
  }
  if (m === 0) {
    const ops: DOp[] = [];
    for (let i = 0; i < n; i++) ops.push({ kind: "-", aIdx: prefix + i, bIdx: 0 });
    return ops;
  }

  const max = n + m;
  const offset = max;
  if (max > 200_000) return opsNaive(a, b, prefix);
  const v = new Int32Array(2 * max + 1);
  const trace: Int32Array[] = [];
  const dCap = Math.max(16, Math.floor((32 << 20) / (16 * max + 8)));

  let found = false;
  for (let d = 0; d <= max; d++) {
    if (d > dCap) return opsNaive(a, b, prefix);
    trace.push(v.slice());
    for (let k = -d; k <= d; k += 2) {
      let x: number;
      if (k === -d || (k !== d && v[offset + k - 1] < v[offset + k + 1])) {
        x = v[offset + k + 1];
      } else {
        x = v[offset + k - 1] + 1;
      }
      let y = x - k;
      while (x < n && y < m && a[x].text === b[y].text) {
        x++;
        y++;
      }
      v[offset + k] = x;
      if (x >= n && y >= m) {
        found = true;
        break;
      }
    }
    if (found) break;
  }

  // Backtrack the trace to reconstruct the (reversed) edit path.
  const rev: DOp[] = [];
  let x = n;
  let y = m;
  for (let d = trace.length - 1; d >= 1; d--) {
    const vv = trace[d];
    const k = x - y;
    let prevK: number;
    if (k === -d || (k !== d && vv[offset + k - 1] < vv[offset + k + 1])) {
      prevK = k + 1;
    } else {
      prevK = k - 1;
    }
    const prevX = vv[offset + prevK];
    const prevY = prevX - prevK;
    while (x > prevX && y > prevY) {
      x--;
      y--;
      rev.push({ kind: " ", aIdx: x + prefix, bIdx: y + prefix });
    }
    if (x === prevX && y === prevY) continue;
    if (x > prevX) {
      x--;
      rev.push({ kind: "-", aIdx: x + prefix, bIdx: 0 });
    } else {
      y--;
      rev.push({ kind: "+", aIdx: 0, bIdx: y + prefix });
    }
  }
  const ops: DOp[] = new Array(rev.length);
  for (let i = 0; i < rev.length; i++) ops[i] = rev[rev.length - 1 - i];
  return ops;
}

// opsNaive is the fallback edit script: remove the whole middle-A block,
// add the whole middle-B block. Linear time, always correct.
function opsNaive(a: DLine[], b: DLine[], prefix: number): DOp[] {
  const ops: DOp[] = [];
  for (let i = 0; i < a.length; i++) ops.push({ kind: "-", aIdx: prefix + i, bIdx: 0 });
  for (let i = 0; i < b.length; i++) ops.push({ kind: "+", aIdx: 0, bIdx: prefix + i });
  return ops;
}

// hunkRange renders one side of a hunk header: bare start when count==1
// (git's convention), "start,count" otherwise.
function hunkRange(start: number, count: number): string {
  return count === 1 ? `${start}` : `${start},${count}`;
}

function writeDline(l: DLine, sign: string): string {
  return l.hasEOL ? sign + l.text + "\n" : `${sign}${l.text}\n\\ No newline at end of file\n`;
}

// writeHunks renders the ops into standard hunks (3 context lines, paired
// line numbers) — the Go engine's grouping rule verbatim.
function writeHunks(a: DLine[], b: DLine[]): string {
  const ops = diffOps(a, b);
  const ctx = DIFF_CONTEXT;
  let out = "";

  type Hunk = { from: number; to: number };
  const hunks: Hunk[] = [];
  let i = 0;
  while (i < ops.length) {
    if (ops[i].kind === " ") {
      i++;
      continue;
    }
    const from = Math.max(0, i - ctx);
    let to = i + 1;
    while (to < ops.length) {
      if (ops[to].kind !== " ") {
        to++;
        continue;
      }
      let j = to;
      while (j < ops.length && ops[j].kind === " ") j++;
      if (j < ops.length && j - to <= 2 * ctx) {
        to = j + 1;
        continue;
      }
      break;
    }
    const end = Math.min(to + ctx, ops.length);
    hunks.push({ from, to: end });
    i = end;
  }

  for (const h of hunks) {
    let countA = 0;
    let countB = 0;
    let startA = 0;
    let startB = 0;
    let first = true;
    for (let o = h.from; o < h.to; o++) {
      const op = ops[o];
      if (first) {
        startA = op.aIdx + 1;
        startB = op.bIdx + 1;
        first = false;
      }
      if (op.kind === " ") {
        countA++;
        countB++;
      } else if (op.kind === "-") {
        countA++;
      } else {
        countB++;
      }
    }
    out += `@@ -${hunkRange(startA, countA)} +${hunkRange(startB, countB)} @@\n`;
    for (let o = h.from; o < h.to; o++) {
      const op = ops[o];
      if (op.kind === " ") out += writeDline(a[op.aIdx], " ");
      else if (op.kind === "-") out += writeDline(a[op.aIdx], "-");
      else out += writeDline(b[op.bIdx], "+");
    }
  }
  return out;
}

// writeWholeHunk renders the whole-file hunk for pure creates and deletes.
function writeWholeHunk(lines: DLine[], sign: "+" | "-"): string {
  let out =
    sign === "+"
      ? `@@ -0,0 +1,${lines.length} @@\n`
      : `@@ -1,${lines.length} +0,0 @@\n`;
  for (const l of lines) out += writeDline(l, sign);
  return out;
}

// computeUnifiedDiff is the TS twin of fileedit.ComputeUnifiedDiff: a
// ground-truth unified diff between two content snapshots. before==null
// means the file did not exist (create); after==null means deletion.
// Byte-identical pairs yield "" (no ledger entry).
export function computeUnifiedDiff(
  before: string | null,
  after: string | null,
  path: string,
): DiffResult {
  const beforeExisted = before !== null;
  const afterExisted = after !== null;
  if (before === null && after === null) {
    return { unifiedDiff: "", kind: "", binary: false, truncated: false };
  }
  if (beforeExisted && afterExisted && before === after) {
    return { unifiedDiff: "", kind: "", binary: false, truncated: false };
  }

  const kind = kindFor(beforeExisted, afterExisted);
  if (isBinary(before) || isBinary(after)) {
    return { unifiedDiff: "", kind, binary: true, truncated: false };
  }

  let header = `--- a/${path}\n+++ b/${path}\n`;
  if (!beforeExisted) header = `--- /dev/null\n+++ b/${path}\n`;
  else if (!afterExisted) header = `--- a/${path}\n+++ /dev/null\n`;

  let body = "";
  if (!beforeExisted) body = writeWholeHunk(splitLines(after as string), "+");
  else if (!afterExisted) body = writeWholeHunk(splitLines(before as string), "-");
  else body = writeHunks(splitLines(before as string), splitLines(after as string));

  let out = header + body;
  let truncated = false;
  if (out.length > MAX_DIFF_BYTES) {
    // The Go engine trims raw bytes; cut at a UTF-8-safe boundary.
    out = [...out].reduce((acc, ch) => (acc.length + ch.length <= MAX_DIFF_BYTES ? acc + ch : acc), "");
    truncated = true;
  }
  return { unifiedDiff: out, kind, binary: false, truncated };
}

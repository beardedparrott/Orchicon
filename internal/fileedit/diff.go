// Package fileedit implements the diff pipeline: server-computed, ground-truth
// unified diffs for every file a session edits. Diffs are computed from real
// file state (snapshot pairs / in-memory before+after), never parsed from tool
// output prose; the ledger rows persist to file_edit_ledger and reconcile
// against the run worktree's git state on completion.
package fileedit

import (
	"bytes"
	"fmt"
)

// Caps and detection thresholds (see architecture-notes/diff-pipeline-file-edit-ledger-server-computed-diffs.md §5).
const (
	// maxDiffBytes caps a single ledger entry's unified diff. Beyond it the
	// diff is cut (truncated=true) so one giant rewrite cannot blow up the
	// ledger, the event stream, or a renderer.
	maxDiffBytes = 256 << 10 // 256 KiB
	// binaryProbeLen is how many leading bytes are probed for a NUL to
	// classify content as binary — git's heuristic.
	binaryProbeLen = 8192
	// diffContext is the number of unchanged context lines shown around each
	// change in a hunk (git's -U3 default).
	diffContext = 3
)

// DiffKind enumerates the ledger entry kinds.
const (
	KindCreate = "create"
	KindModify = "modify"
	KindDelete = "delete"
)

// DiffResult is the outcome of ComputeUnifiedDiff for one file.
type DiffResult struct {
	// UnifiedDiff is the unified diff text ("--- a/<path>" …), 3 context
	// lines, LF line endings, empty for binary files.
	UnifiedDiff string
	// Kind is create | modify | delete.
	Kind string
	// Binary reports NUL-in-first-8KiB on either side (empty diff).
	Binary bool
	// Truncated reports the diff text was cut at maxDiffBytes.
	Truncated bool
}

// ComputeUnifiedDiff computes a ground-truth unified diff between two file
// content snapshots. before==nil means the file did not exist (create);
// after==nil means it was deleted. Byte-identical pairs yield "" (no entry):
// callers skip ledger rows for no-op edits. UTF-8 passes through untouched —
// diffing is strictly line-level, never byte- or rune-rewriting.
func ComputeUnifiedDiff(before, after []byte, path string) DiffResult {
	beforeExisted := before != nil
	afterExisted := after != nil
	if before == nil && after == nil {
		return DiffResult{}
	}
	if beforeExisted && afterExisted && bytes.Equal(before, after) {
		return DiffResult{} // no-op edit
	}

	kind := kindFor(beforeExisted, afterExisted)
	if isBinary(before) || isBinary(after) {
		return DiffResult{Kind: kind, Binary: true}
	}

	// Header: git-style a/ b/ prefixes; /dev/null for pure create/delete.
	header := fmt.Sprintf("--- a/%s\n+++ b/%s\n", path, path)
	if !beforeExisted {
		header = fmt.Sprintf("--- /dev/null\n+++ b/%s\n", path)
	} else if !afterExisted {
		header = fmt.Sprintf("--- a/%s\n+++ /dev/null\n", path)
	}

	var b bytes.Buffer
	b.WriteString(header)
	switch {
	case !beforeExisted:
		writeWholeHunk(&b, '+', splitLines(after)) // "@@ -0,0 +1,N @@"
	case !afterExisted:
		writeWholeHunk(&b, '-', splitLines(before)) // "@@ -1,N +0,0 @@"
	default:
		writeHunks(&b, splitLines(before), splitLines(after))
	}

	out := b.String()
	truncated := false
	if len(out) > maxDiffBytes {
		out = out[:maxDiffBytes]
		truncated = true
	}
	if out == "" && !truncated {
		return DiffResult{} // no-op edit: nothing changed
	}
	return DiffResult{UnifiedDiff: out, Kind: kind, Truncated: truncated}
}

// ReconcileDiff is ComputeUnifiedDiff under the plan's reconciliation name —
// the git-reconciliation path computes diffs with the identical engine.
func ReconcileDiff(before, after []byte, path string) DiffResult {
	return ComputeUnifiedDiff(before, after, path)
}

func kindFor(beforeExisted, afterExisted bool) string {
	switch {
	case !beforeExisted && afterExisted:
		return KindCreate
	case beforeExisted && !afterExisted:
		return KindDelete
	default:
		return KindModify
	}
}

// isBinary applies git's heuristic: a NUL byte in the first 8 KiB.
func isBinary(b []byte) bool {
	if len(b) > binaryProbeLen {
		b = b[:binaryProbeLen]
	}
	return bytes.IndexByte(b, 0) >= 0
}

// dline is one diffable line: its text (without the trailing newline) and
// whether the original content had a newline after it.
type dline struct {
	text   string
	hasEOL bool
}

// splitLines splits content into lines. A trailing "\n" does NOT produce an
// extra empty line — n lines = n "\n"-separated fragments, the convention
// unified diffs count with. The final fragment without a newline keeps
// hasEOL=false so the "\ No newline at end of file" marker can be emitted.
func splitLines(c []byte) []dline {
	if len(c) == 0 {
		return nil
	}
	out := make([]dline, 0, bytes.Count(c, []byte("\n"))+1)
	start := 0
	for i := 0; i < len(c); i++ {
		if c[i] == '\n' {
			out = append(out, dline{text: string(c[start:i]), hasEOL: true})
			start = i + 1
		}
	}
	if start < len(c) {
		out = append(out, dline{text: string(c[start:]), hasEOL: false})
	}
	return out
}

// op is one edit-script operation: ' ' keep, '-' remove, '+' add.
// For ' ' both indices are set; for '-' only aIdx; for '+' only bIdx.
type op struct {
	kind byte
	aIdx int
	bIdx int
}

// diffOps computes a minimal edit script via Myers' O((N+M)D) greedy
// algorithm (the same algorithm git diff uses). Both sides may be empty.
func diffOps(a, b []dline) []op {
	n, m := len(a), len(b)
	// Trim the common prefix/suffix first — the common case (a small edit in
	// a big file) then costs almost nothing.
	p := 0
	for p < n && p < m && a[p].text == b[p].text {
		p++
	}
	s := 0
	for s < n-p && s < m-p && a[n-1-s].text == b[m-1-s].text {
		s++
	}
	var ops []op
	for i := 0; i < p; i++ {
		ops = append(ops, op{kind: ' ', aIdx: i, bIdx: i})
	}
	ops = append(ops, opsMyers(a[p:n-s], b[p:m-s], p, s)...)
	for i := 0; i < s; i++ {
		ops = append(ops, op{kind: ' ', aIdx: n - s + i, bIdx: m - s + i})
	}
	return ops
}

// opsMyers runs Myers' greedy algorithm on the middle (non-trimmed) section
// and returns ops with absolute indices (prefix lines already consumed).
// A budget guard falls back to a naive whole-middle replace when the edit
// distance explodes (a near-total rewrite): Myers' O(D·(N+M)) work and
// per-round trace snapshots are unbounded otherwise, and for a near-total
// rewrite the naive "-"-all/"+"-all script IS what a minimal diff renders
// anyway. Correctness is unchanged — only minimality is given up, and the
// 256 KiB entry cap bounds the text either way.
func opsMyers(a, b []dline, prefix, suffix int) []op {
	n, m := len(a), len(b)
	if n == 0 && m == 0 {
		return nil
	}
	if n == 0 {
		ops := make([]op, 0, m)
		for i := range b {
			ops = append(ops, op{kind: '+', bIdx: prefix + i})
		}
		return ops
	}
	if m == 0 {
		ops := make([]op, 0, n)
		for i := range a {
			ops = append(ops, op{kind: '-', aIdx: prefix + i})
		}
		return ops
	}

	// Effort guard: work ≈ D·(N+M); cap trace snapshots ≈ 32 MiB.
	max := n + m
	offset := max
	dCap := (32 << 20) / (16*max + 8) // ints per round: (2max+1)·8B
	if dCap < 16 {
		dCap = 16
	}
	if max > 200_000 {
		return opsNaive(a, b, prefix)
	}
	v := make([]int, 2*max+1)
	var trace [][]int

find:
	for d := 0; d <= max; d++ {
		if d > dCap {
			return opsNaive(a, b, prefix)
		}
		snap := make([]int, len(v))
		copy(snap, v)
		trace = append(trace, snap)
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[offset+k-1] < v[offset+k+1]) {
				x = v[offset+k+1]
			} else {
				x = v[offset+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x].text == b[y].text {
				x++
				y++
			}
			v[offset+k] = x
			if x >= n && y >= m {
				break find
			}
		}
	}

	// Backtrack the trace to reconstruct the (reversed) edit path.
	var rev []op
	x, y := n, m
	for d := len(trace) - 1; d >= 0; d-- {
		if d == 0 {
			// The origin IS the distance-0 endpoint — every op was already
			// consumed by rounds d>=1. Computing prevK against the zeroed
			// snapshot here would fabricate phantom ops (and negative
			// indices) for shared prefixes.
			continue
		}
		vv := trace[d]
		k := x - y
		var prevK int
		if k == -d || (k != d && vv[offset+k-1] < vv[offset+k+1]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := vv[offset+prevK]
		prevY := prevX - prevK
		for x > prevX && y > prevY { // diagonal (keep) steps
			x--
			y--
			rev = append(rev, op{kind: ' ', aIdx: x + prefix, bIdx: y + prefix})
		}
		if x == prevX && y == prevY {
			continue
		}
		if x > prevX { // horizontal step = removal from a
			x--
			rev = append(rev, op{kind: '-', aIdx: x + prefix})
		} else { // vertical step = addition from b
			y--
			rev = append(rev, op{kind: '+', bIdx: y + prefix})
		}
	}
	// The backtrack produced reversed ops; restore order.
	ops := make([]op, len(rev))
	for i := range rev {
		ops[i] = rev[len(rev)-1-i]
	}
	return ops
}

// opsNaive is the fallback edit script: remove the whole middle-A block,
// add the whole middle-B block. Linear time, always correct.
func opsNaive(a, b []dline, prefix int) []op {
	ops := make([]op, 0, len(a)+len(b))
	for i := range a {
		ops = append(ops, op{kind: '-', aIdx: prefix + i})
	}
	for i := range b {
		ops = append(ops, op{kind: '+', bIdx: prefix + i})
	}
	return ops
}

// writeHunks renders the ops into standard hunks (3 context lines, paired
// line numbers, "\ No newline at end of file" markers) following git's
// grouping rule: two changes land in the same hunk while the unchanged gap
// between them is ≤ 2*diffContext lines.
func writeHunks(w *bytes.Buffer, a, b []dline) {
	ops := diffOps(a, b)
	ctx := diffContext

	// Group the ops into hunks: walk the op list, opening a hunk at the
	// first change and extending it while another change arrives within
	// 2*ctx keeps of the previous one.
	type hunk struct{ from, to int } // [from, to) into ops
	var hunks []hunk
	i := 0
	for i < len(ops) {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		// Open a hunk with up to ctx keeps before the first change.
		from := i - ctx
		if from < 0 {
			from = 0
		}
		to := i + 1
		// Extend while the next change is within 2*ctx of the last change.
		for to < len(ops) {
			if ops[to].kind != ' ' {
				to++
				continue
			}
			// Count keeps until the next change (or end).
			j := to
			for j < len(ops) && ops[j].kind == ' ' {
				j++
			}
			if j < len(ops) && j-to <= 2*ctx {
				to = j + 1 // absorb the keeps + the next change
				continue
			}
			break
		}
		// Close the hunk with up to ctx keeps after the last change.
		end := to + ctx
		if end > len(ops) {
			end = len(ops)
		}
		hunks = append(hunks, hunk{from: from, to: end})
		i = end
	}

	for _, h := range hunks {
		// Hunk header line numbers: the first A-consuming op fixes startA,
		// the first B-consuming op fixes startB. A hunk of only additions
		// (only deletions) starts at the A (B) cursor position with count 0
		// — git's convention, matching "@@ -0,0 +1,N @@" for creates.
		countA, countB := 0, 0
		var startA, startB int
		first := true
		for _, o := range ops[h.from:h.to] {
			switch o.kind {
			case ' ':
				countA++
				countB++
				if first {
					startA, startB = o.aIdx+1, o.bIdx+1
					first = false
				}
			case '-':
				countA++
				if first {
					startA, startB = o.aIdx+1, o.bIdx+1
					first = false
				}
			case '+':
				countB++
				if first {
					startA, startB = o.aIdx+1, o.bIdx+1
					first = false
				}
			}
		}
		// Git's convention: a count of 1 is printed bare ("-1", "+1"),
		// larger counts carry ",N" ("-1,3"). Verified against `git diff`.
		header := fmt.Sprintf("@@ -%s +%s @@\n",
			hunkRange(startA, countA), hunkRange(startB, countB))
		w.WriteString(header)
		for _, o := range ops[h.from:h.to] {
			switch o.kind {
			case ' ':
				w.WriteByte(' ')
				writeDline(w, a[o.aIdx])
			case '-':
				w.WriteByte('-')
				writeDline(w, a[o.aIdx])
			case '+':
				w.WriteByte('+')
				writeDline(w, b[o.bIdx])
			}
		}
	}
}

// writeWholeHunk renders the whole-file hunk for pure creates ('+' side,
// "@@ -0,0 +1,N @@") and pure deletes ('-' side, "@@ -1,N +0,0 @@").
func writeWholeHunk(w *bytes.Buffer, sign byte, lines []dline) {
	if sign == '+' {
		fmt.Fprintf(w, "@@ -0,0 +1,%d @@\n", len(lines))
	} else {
		fmt.Fprintf(w, "@@ -1,%d +0,0 @@\n", len(lines))
	}
	for _, l := range lines {
		w.WriteByte(sign)
		writeDline(w, l)
		if w.Len() > maxDiffBytes+1024 { // engine hard-stop; entry cap trims below
			break
		}
	}
}

// writeDline writes the line text plus newline, emitting the standard
// "\ No newline at end of file" marker for a final fragment without one.
func writeDline(w *bytes.Buffer, l dline) {
	w.WriteString(l.text)
	if l.hasEOL {
		w.WriteByte('\n')
		return
	}
	w.WriteString("\n\\ No newline at end of file\n")
}

// hunkRange renders one side of a hunk header: bare start when count==1
// (git's convention), "start,count" otherwise.
func hunkRange(start, count int) string {
	if count == 1 {
		return fmt.Sprintf("%d", start)
	}
	return fmt.Sprintf("%d,%d", start, count)
}

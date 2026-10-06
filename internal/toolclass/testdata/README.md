# `rollup_fixture.json` — the cross-client contract

This file is read **verbatim** by two tests in two languages:

- `internal/toolclass/summarize_test.go` (Go, this repo) — `os.ReadFile("testdata/rollup_fixture.json")`;
- the GUI's Vitest test (work item child 5) — the TS mirror of `toolclass.Summarize`, asserted over
  these same cases so the terminal and the browser cannot render different numbers from the same
  ledger JSON. Precedent: `internal/tui/chat/grouping_test.go:5-6`, which ports
  `frontend/src/components/executions/sessionItems.test.ts`'s cases verbatim.

Rules for anyone touching it:

- **`ts`, `now` and `window` are epoch MILLISECONDS** (`ts` on a ledger entry, `now` and `window` on
  a case). JS has no `time.Duration`; ms is the one unit both sides can express exactly.
- **`want` is the exact output string** of `toolclass.Summarize(ledger, now, window)`, separator
  ` · ` (U+00B7). Nothing trims or reformats it.
- An entry with an **absent `ts`** or **`ts: 0`** is in no window and is skipped.
- The window is **inclusive** on the left: an entry exactly `window` ms old is counted.
- `window <= 0` falls back to `toolclass.DefaultWindow` (30s), on both sides.
- **Do not move, copy or regenerate this file per client.** A Go `testdata/` directory is the one
  place `go test` reads without a path hack, and a second copy is a second thing to drift.

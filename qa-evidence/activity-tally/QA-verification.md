# QA verification — activity tally under-report

QA Engineer (step 4). Verified commit `6d54369b` (on top of the SSE's `862bd167`) against every
acceptance criterion, fixed nothing (no defect found), and re-captured the live e2e evidence so no
stale wording ships.

## Suites run (all green)

| Suite | Result |
|---|---|
| `go build ./...` | OK |
| `go test ./internal/toolclass/ ./internal/tui/... ./internal/askorchicon/` | all `ok` (askorchicon incl. DB-backed) |
| `npx vitest run` (frontend) | **796/796 passed**, 77 files |
| `npx tsc -b` | clean |
| `semgrep --config auto --error` on changed `.go`/`.ts`/`.tsx` | 0 findings |

## Independent measurement (AC1/AC2) — not taken on trust

Ran the REAL `toolclass.Classify` over the REAL product registry (`askorchicon.NewToolRegistry`):

```
REGISTRY total=85 modify=0 read=0 bash=0 other=84 ignore=1   (only `ask_user` ignored)
HOSTSUITE ignore=1 other=0                                     (todowrite)
```

So the fix counts 84 of 85 product tools that the old unknown-name default silently dropped. The
operator's 3-bash + 9-product-call turn renders **before** `3 bash · last 0s` → **after**
`3 bash · 9 other tools · newest call 0s ago` — asserted live by `TestTheOperatorTurnIsFullyCounted`
over real `recordStart`/`snapshot()` bytes.

## AC11 / AC12 — mutation tests, both proven RED then restored

`chathistory.go`'s `askToolCallJSON` is the only struct RE-MARSHAL over the `tool_calls` column.
Deleting its `IssuedAtUnixMs` field and re-running:

- `TestToolLedgerSurvivesTheTerminalSanitizer` → **RED** (`issued_at_unix_ms = 0 after the terminal repair`).
- `TestLiveToolLedgerPersistsCallsAndResults` (DB round-trip, finished row) → **RED**
  (`the finished row's tool_calls lost their issue stamp`).

Restored → both green. Every other path over the column (`toolCallsFromJSON`, `reloadSnapshot`,
`sanitizeHistoryRows`'s per-turn history read, the native session file, compaction, `messageRowToProto`)
either passes through untouched or re-marshals through the now-stamp-carrying struct; `toolCallsFromJSON`
is separately guarded by `TestToolCallsFromJSONCarriesTheStamp`/`TestTheIssueStampSurvivesTheWire`.

## Live e2e (surface-impact) — re-captured

The activity line is user-visible in BOTH the TUI footer and the GUI status region, so the change
was verified on both LIVE surfaces through the real harness (real `bin/orch` in a real pty; real SPA
in headless Chromium):

- TUI leg `TestActivityLineE2E` (+ `TurnEnd`, `ZeroToolCalls`) — **PASS**; re-captured frames now read
  `… · 1 modify · 2 reads · 1 bash · newest call 1s ago` (previously the stale `· last 1s`).
- GUI leg `frontend/tests/activity-line-e2e.spec.ts` — **2/2 PASS**; `gui-activity-line.txt` reads
  `Orchicon is ruminating… · 1 modify · 2 reads · 1 bash · newest call 1s ago`, and the cross-client
  handshake (`tui-cross-client.json`) confirms both clients drew the same word and counts at the same
  server stamp.
- AC10: exactly **1** frame row carries the activity line in every captured frame (with counters and
  with none) — the one-row budget holds.

## Criteria verdict

1 measured ✓ (evidence `AC1-measurement.md` + independent registry run above, before/after stated);
2 counts the real work ✓ (84/85 counted; `3 bash` → `3 bash · 9 other tools`); 3 `ask_user`/`permission.*`
uncounted ✓ (`TestClassifyAskUserNeverCounted`, `TestClassifyPermissionRecordsNeverCounted`); 4 parity ✓
(17-case fixture asserted from both sides, both suites green); 5 empty case ✓; 6 fixed ordering ✓; 7 age
reads as "newest call Ns ago" not a window ✓; 8 30s window unchanged ✓; 9 no clock/plumbing change ✓
(`tool_ledger.go`, `service.go`, proto, `controller.go`, `toolCallsFromMessages` all untouched in the
diff); 10 row budget ✓; 11 candidate D checked + mutation-proven ✓; 12 finished row stamped ✓.

No defect found; nothing to fix.

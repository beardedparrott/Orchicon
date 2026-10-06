# AC1 measurement — a real turn, its raw ledger, and both clients' renders

Produced by the Senior Software Engineer (step 2) with REAL code: the ledger bytes come from the
real `toolLedger.recordStart`/`snapshot()` (`internal/askorchicon`), classified by the real
`toolclass.Classify`, and rendered by the real `toolclass.Summarize` (Go/TUI) and the real
`summarizeToolCalls` (`frontend/src/lib/ask-tool-summary.ts`, GUI). No string in the "after"
column is retyped.

The "before" column is the SAME ledger through the classifier as it stood at `f684ac5c`
(`git show HEAD:internal/toolclass/toolclass.go`) — the pre-change default that made an
unrecognised name `Ignore`.

## 1. The turn: 3 bash + 9 real native-Ask PRODUCT tool calls

The native Ask surface emits product tools under their BARE names (`native_tools.go:84`: the
registry is keyed by bare name; 0 of 86 entries carry the `orchicon_` prefix). The 9 product names
below are registered in `internal/askorchicon/tools.go`. `recordStart` was called once per name at a
single clock instant, so the ledger is a genuine snapshot:

```
[{"id":"tc-1","type":"function","function_name":"bash","arguments":"{}","issued_at_unix_ms":1700000000000},
 {"id":"tc-2","type":"function","function_name":"bash","arguments":"{}","issued_at_unix_ms":1700000000000},
 {"id":"tc-3","type":"function","function_name":"bash","arguments":"{}","issued_at_unix_ms":1700000000000},
 {"id":"tc-4","type":"function","function_name":"list_projects","arguments":"{}","issued_at_unix_ms":1700000000000},
 {"id":"tc-5","type":"function","function_name":"list_work_items","arguments":"{}","issued_at_unix_ms":1700000000000},
 {"id":"tc-6","type":"function","function_name":"get_work_item","arguments":"{}","issued_at_unix_ms":1700000000000},
 {"id":"tc-7","type":"function","function_name":"read_project_file","arguments":"{}","issued_at_unix_ms":1700000000000},
 {"id":"tc-8","type":"function","function_name":"list_executions","arguments":"{}","issued_at_unix_ms":1700000000000},
 {"id":"tc-9","type":"function","function_name":"get_execution","arguments":"{}","issued_at_unix_ms":1700000000000},
 {"id":"tc-10","type":"function","function_name":"list_ideas","arguments":"{}","issued_at_unix_ms":1700000000000},
 {"id":"tc-11","type":"function","function_name":"get_project","arguments":"{}","issued_at_unix_ms":1700000000000},
 {"id":"tc-12","type":"function","function_name":"list_audit_events","arguments":"{}","issued_at_unix_ms":1700000000000}]
```

Every row carries `issued_at_unix_ms = 1700000000000` — the stamps are present, so nothing is
skipped for a missing time (AC11/AC12 hold on this turn's path).

## 2. Counted vs dropped, BY NAME

| call | class BEFORE | class AFTER |
|---|---|---|
| `bash` ×3 | `Bash` (counted) | `Bash` (counted) |
| `list_projects` | **`Ignore` (DROPPED)** | `Other` (counted) |
| `list_work_items` | **`Ignore` (DROPPED)** | `Other` (counted) |
| `get_work_item` | **`Ignore` (DROPPED)** | `Other` (counted) |
| `read_project_file` | **`Ignore` (DROPPED)** | `Other` (counted) |
| `list_executions` | **`Ignore` (DROPPED)** | `Other` (counted) |
| `get_execution` | **`Ignore` (DROPPED)** | `Other` (counted) |
| `list_ideas` | **`Ignore` (DROPPED)** | `Other` (counted) |
| `get_project` | **`Ignore` (DROPPED)** | `Other` (counted) |
| `list_audit_events` | **`Ignore` (DROPPED)** | `Other` (counted) |

**9 of 12 calls were dropped, all by the unknown-name default.** Not because of the `orchicon_`
prefix: the names are bare, so the prefix strip never applies. This is the under-report.

## 3. The two clients' rendered lines

| | Go / TUI (`toolclass.Summarize`) | GUI (`summarizeToolCalls`) |
|---|---|---|
| **BEFORE** | `3 bash · last 0s` | `3 bash · last 0s` |
| **AFTER** | `3 bash · 9 other tools · newest call 0s ago` | `3 bash · 9 other tools · newest call 0s ago` |

Byte-identical across clients before and after (the shared fixture asserts this for 17 cases in
both languages). The operator's screenshot showed `3 bash` on a twelve-call turn — exactly the
BEFORE column.

BEFORE numbers come from the real pre-change classifier run over the real ledger bytes
(`/tmp/orchicon/before`, output: `BEFORE counts: 0 modify, 0 read, 3 bash; dropped by name: [list_projects list_work_items get_work_item read_project_file list_executions get_execution list_ideas get_project list_audit_events]`).
AFTER comes from `TestTheOperatorTurnIsFullyCounted` (`internal/askorchicon/tool_ledger_summarize_test.go`).

## 4. Cross-client parity over the shared fixture (AC4)

`frontend/src/lib/ask-tool-summary.test.ts` enumerates every case of
`internal/toolclass/testdata/rollup_fixture.json` and asserts `summarizeToolCalls` equals the
fixture's `want`; `internal/toolclass/summarize_test.go` asserts `Summarize` equals the SAME `want`.
Both suites pass (17 fixture cases). See `gui-renders.txt` for the GUI port's output per case,
produced by the shipped TS port.

## 5. Prior real captures (context, not this turn)

`qa-evidence/activity-line-e2e/` holds a real server ledger and both clients' renders from the
feature's earlier e2e leg. That turn used only HOST-SUITE tools (`read`, `batch_grep`, `bash`,
`write`), so it exercised neither the defect nor the fix; its counter `1 modify · 2 reads · 1 bash`
is unchanged, but its `last Ns` suffix predates this wording change and must be RE-CAPTURED by the
QA step (step 4) via the e2e harness. It is left here as the historical capture it is.

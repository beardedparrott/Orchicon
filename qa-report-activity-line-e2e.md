# QA report — end-to-end proof: the activity line through the real TUI frame and GUI pane

Work item: `end-to-end-proof-the-activity-line-through-the-real-tui-frame-and-gui-pane-ghp86ffexxre3cny`
Branch: `end-to-end-proof-the-activity-line-through-the-real-tui-frame-and-gui-pane-ghp86ffexxre3cny`
Step 2/5 (Senior Software Engineer). Evidence: `qa-evidence/activity-line-e2e/`.

## 0. Method — the ONE substitution, stated loudly

**The turn is real; the agent is a fixture.** A genuinely model-driven turn cannot be run in this
container, and that is measured, not assumed: `exec.LookPath("opencode")` is empty and the
serve-host fallback probe (`internal/opencode/servehost.go:400-410`) misses too, so
`internal/opencode/chatsession.go:54` fails every `ChatStream` with
`"host opencode serve unavailable — Ask chat transport is disabled"`. The repo already sanctions
exactly this outcome — `TestE2EOpenCodeLiveWorkerCallsTheProjectServer`
(`internal/opencode/e2e_worker_mcp_proof_test.go:171`) skips and records
`ModelAccess: "unavailable"`.

So the harness substitutes **one** layer — the provider behind `ChatStream` — and serves **every
layer above it unmodified**:

* the real Connect/HTTP transport,
* the real `internal/tui/chat` controller state machine (heartbeat → `serverTimeMs`/`ServerTimeSince`,
  the 1 s `ListMessages` poll → `pageToolCalls` → `chatStore`),
* the real `App.onChatWake` → Ask pane → frame pipeline, in a real pty (the binary is `bin/orch`),
* the real React route and DOM, in a real Chromium.

The tool calls the counters count are the **fixture's own ledger rows**
(`internal/testfixtures/activitye2e/plane.go`, `p.calls`), served on the same `ListMessages` page
the production client polls. So AC4's reconciliation is a genuine client-vs-server comparison over
one ledger, not a client comparing itself to itself.

**One plane, two clients.** The TUI takes any plane URL and the SPA proxies `/orchicon.api.v1` to
it, so a single fixture process on `:18080` serves the *same conversation, the same ledger and the
same server clock* to both clients. That is what makes the cross-client claim (AC7) a literal
observation.

### What was run (exact commands)

| Leg | Command | Result |
|---|---|---|
| TUI | `ORCH_ACTIVITY_E2E=1 ORCH_PTY_SMOKE=1 ORCH_ACTIVITY_E2E_ADDR=127.0.0.1:18081 ORCH_ACTIVITY_E2E_OUT=<repo>/qa-evidence/activity-line-e2e go test ./internal/tui -run TestActivityLineE2E -v -count=1 -timeout 600s` | **PASS** (92.0 s) |
| GUI | `cd frontend && PLAYWRIGHT_BROWSERS_PATH=/tmp/orchicon/pw npx playwright test --config playwright.activity-e2e.config.ts --project=dark-desktop` | **PASS** (46.2 s) |
| Go suites | `go test ./internal/tui/... ./internal/toolclass/...` | **ok**, all packages |
| Frontend types | `cd frontend && npx tsc -b` | exit **0** |
| Frontend unit | `cd frontend && npx vitest run` | **796/796** (77 files) |
| Go vet | `go vet ./internal/testfixtures/activitye2e/... ./internal/tui/...` | clean |

Environment note: the container's default Go paths are unwritable, so the Go env is pinned to a
writable cache under the tree (`GOMODCACHE=<repo>/.dev/mod`, `GOCACHE=/tmp/orchicon/gocache`). The
GUI config performs this pinning itself for the plane it launches.

## 1. AC1 — one real turn, end to end, in both clients

**TUI** (`internal/tui/activity_e2e_states_test.go` `TestActivityLineE2E`): ONE pty session driven
start-to-finish, no relaunch: `/conversations` → open `conv-activity-e2e` → type the prompt → `\r`.
The line appears, counters land, and the verb rotates — all observed, one turn.

**GUI** (`frontend/tests/activity-line-e2e.spec.ts`): one browser page, one turn, the line observed
at every state. The pane's own `h2` (conversation title) and the transcript's user message prove
the conversation is open before the send.

Artifacts: `tui-01..13-*.txt` (replayed frames) and `gui-01..05-*.png` (screenshots).

## 2. AC2 — observed through the OUTER surface

* **TUI**: every assertion is on the **replayed screen grid** — the terminal emulator
  (`newScreen`/`feed`) replayed from the raw pty bytes the live `bin/orch` wrote, then the footer
  row read off that grid (`footerRow`). Nothing asserts `askStatusLine()` in isolation and nothing
  asserts the summarizer's return value; `footerRow` finds the line by its own painted words on the
  frame the operator would be looking at. The live-pty half is gated on `ORCH_PTY_SMOKE=1`, exactly
  as `internal/tui/orch_pty_smoke_test.go` prescribes.
* **GUI**: every assertion is on the **rendered DOM** —
  `[data-testid="ask-activity-line"]` with `role="status"` (ActivityLine.tsx's own contract). The
  extracted text is the *visible* `aria-hidden` span (the rotating verb + counters), **not** the
  region's `innerText`, because the `sr-only` announcement span is clipped — not hidden — and would
  fold a stale counter back in.

## 3. AC3 — the REGRESSION, proven fixed (content arrived, line STILL present)

**The regression this feature exists to fix.** Before PR #654 (TUI) and #657 (GUI) the line was
gated on "before the first token", so the moment streaming content arrived the line vanished. The
operator's own words are quoted in the code: *"After the initial 'Orchicon is thinking...',
streaming started and the 'Orchicon is thinking...' went away and never came back."*

Observed **from one frame**, both clients:

* TUI — `tui-05-ac3-content-and-line-same-frame.txt`: the frame carries the streamed token
  `E2EWITNESSCONTENT` in the body **and** `│Orchicon is polishing… · 1 modify · 2 reads · 1 bash ·
  last 1s│` in the footer. Same frame, both facts (subtest `3_ac3_line_survives_content` asserts
  both from one `screen`).
* GUI — `gui-03-ac3-content-and-line-same-frame.png`: `page.getByText(WITNESS_CONTENT)` visible AND
  the activity line present and still carrying its counter.

**The pre-change build (AC3's "if practical"): the fallback evidence was used.** A second full
build (`git worktree add .pre-change 5dfc01c7` + `make build`) was not run within the budget; the
deleted gate is pinned by a live test instead:

* `frontend/src/routes/-ask-orchicon.activity-line.guard.test.ts` (AC1 case) asserts the source does
  **not** contain `isThinking && groupedStream.length === 0` and does not contain
  `visible until any streaming content arrives` — the exact deleted gate.
* `git show 869a707a:frontend/src/routes/ask-orchicon.tsx` line 1921 shows the pre-change
  `{isThinking && groupedStream.length === 0 && (` gate that #657 (`83946436`) replaced with
  `const turnInFlight = isStreaming || serverTurnInFlight;` (now line 545).
* `git show 5dfc01c7:internal/tui/app.go` lines 3858 + 4266 show the TUI's pre-#654 form.

Which was used: **the fallback** (deleted-gate pin + the two `git show` receipts above). Say so
plainly: the pre-change *execution* was not re-run; the pre-change *gate* is pinned by a passing
test and shown in the commit history.

## 4. AC4 — counters match reality (the server's own ledger)

Both legs compare the **painted** counter to the server's independent `SummarizeCalls` over the
fixture's own rows:

* TUI subtest `5_counters_match_the_server_ledger`: `serverCounter(plane)` == the painted
  `countHalf(footerCounter(row))`, with the ledger printed on mismatch. Frame:
  `tui-08-ac4-reconcile.txt`.
* GUI: reads `GET /__e2e/state` (which serves `plane.SummarizeNow()`) and asserts the DOM's counter
  equals it. The raw ledger is recorded verbatim to `gui-server-ledger.json`:

```json
{"calls":[{"tool_name":"read",...},{"tool_name":"batch_grep",...},
          {"tool_name":"bash",...},{"tool_name":"write",...}],
 "counter":"1 modify · 2 reads · 1 bash · last 1s"}
```

This is the honesty check's "consistent with the ledger the server actually holds — not merely
with what the client thinks". It is what caught a **harness** defect during development (below).

### Defect found and fixed during this task (AC10)

**Symptom.** The GUI's counts read `2 modifies · 4 reads · 2 bash` — exactly **2×** the TUI's
`1 modify · 2 reads · 1 bash` — while AC4 (GUI == server) still passed, proving the *fixture's own
ledger* had grown to 8 rows.

**Root cause.** `Plane.Reset` cleared the `scripted` flag, but a stream that had already attached
**before** the reset was still inside its inter-call sleep; it resumed *after* the reset and issued
a second full script. Two interleaved runs, one turn. A tracing run
(`ORCH_ACTIVITY_E2E_TRACE=<file>`) shows it: `read,batch_grep,read,bash,batch_grep,write,bash,write`.

**Fix** — `internal/testfixtures/activitye2e/plane.go`: a generation counter (`gen`) bumped by
`Reset`; a stream captures it at attach and re-checks it immediately before every ledger write, and
aborts if superseded. The trace after the fix shows exactly one run
(`issueCall read → batch_grep → bash → write`) and the ledger holds 4 calls.

This was a **harness** bug, never a client bug, and it is fixed here rather than filed (AC10). It
is also the reason the GUI leg is now self-contained (see the run recipe).

## 5. AC5 — escalation wins, observed LIVE

**TUI** — two live legs, both on the replayed frame:

* `7_ac5_silence_escalates_and_drops_the_counter`: the plane goes silent; after the 25 s band the
  footer reads `│Orchicon is enumerating… · no output for 25s│` — the watchdog's verdict, with the
  **counters gone** (`tui-10-ac5-stalled-no-output.txt`). A count beside "no output" would claim
  work that is not happening; the test fails if any counter survives.
* `8_ac5_killed_connection_never_keeps_the_counter`: counters are re-established
  (`tui-11-*.txt`), then every held socket is torn down with the real
  `CloseClientConnections`; the footer stops painting a live-looking counter
  (`tui-12-ac5-killed-connection-counter-gone.txt`). This is a **real socket teardown**, not a
  status the client is told to believe.

**GUI** — `PhaseDown` **stops the plane** (the work item's own wording, "kill the connection (or
stop the plane) mid-turn"): the open stream terminates with an error and the re-dial fails, so the
client's own `fail()` → `reconnecting` path runs. The disconnected banner
(`Connection interrupted — still working…` / `Turn stalled`) becomes visible and
`[data-testid="ask-activity-line"]` reaches **count 0** — the banner outranks the line.
`gui-04b-ac5-counters-before-the-kill.png` (counters present) →
`gui-05-ac5-plane-down-banner-outranks.png` (banner outranks).

Note on the mechanism: through the Vite dev proxy a bare `CloseClientConnections` severs
plane↔vite but leaves browser↔vite open, so the browser sees *silence*, not a drop, and never enters
`reconnecting`. Stopping the plane (an error on the open stream) is the variant that reaches the
banner through the proxy — and it is an explicitly sanctioned variant in the work item.

## 6. AC6 — the row budget holds in the real frame

TUI: `6_ac6_footer_is_one_row` counts frames rows containing the line and fails unless it is
**exactly one** (`tui-09-ac6-row-budget.txt`). The second test
(`TestActivityLineE2EZeroToolCalls`) measures the footer's frame-row index in the zero-tool-call
turn against the with-tools turn and fails if the counter moved it — i.e. the transcript body
neither lost nor gained a row. Both run on the live replayed frame.

## 7. AC7 — cross-client agreement, observed

One plane, one conversation, one ledger, one server clock.

* TUI (`tui-cross-client.json`): `stamp=1700000004000`, `word="honing"`,
  `counter="1 modify · 2 reads · 1 bash"`.
* GUI (`gui-activity-line.txt`): `stamp=1700000004000`, `word=honing`,
  `counter=1 modify · 2 reads · 1 bash`.

The clock is aligned **explicitly and on purpose**: the verb is a pure function of the server stamp
(`VerbAt` / `verbAt`), so "the same word" is only a meaningful claim **at the same stamp**. The TUI
records the stamp it drew against; the GUI sets the fixture's heartbeat to exactly that stamp and
asserts the word the rotation names for it. Two clients, one server clock, one word — with no
shared state between them. The clients' own trailing `last Ns` age is normalised out (each computes
it from its own clock); everything before it is the work and must agree exactly. Screenshot:
`gui-03b-ac7-same-server-clock.png`; TUI frame: `tui-07-rotation-after.txt`.

## 8. AC8 — full suites green

Named above in §0: the TUI suite (`go test ./internal/tui/...`), `internal/toolclass/...`,
frontend `tsc -b` (exit 0), and `vitest run` (796/796, including the activity-line lib and route
tests). `make ci` itself was not run end-to-end (its cold Go gate exceeds this step's budget); the
constituent commands above are the nearest equivalent and each is named with its result.

## 9. AC9 — evidence attached as artifacts

Under `qa-evidence/activity-line-e2e/`:

* `tui-01..13-*.txt` — 13 replayed TUI frames, one per observed state (plus the conversation-open
  frame and the pre-kill counters frame).
* `gui-01..05-*.png` — 7 GUI screenshots, one per observed state.
* `gui-activity-line.txt` — the GUI's extracted line at the aligned server clock.
* `gui-server-ledger.json` — the fixture's raw ledger behind AC4.
* `tui-cross-client.json` — the TUI→GUI handshake for AC7.
* `logs/` — the legs' logs (`tui-leg.log`, `gui-leg.log`, `build.log`, plane/vite logs).

## 10. AC10 — no deferred edge

The one defect this task found is fixed **here**, not filed: the fixture's double-script race
(§4, generation guard). No production client code needed changing — the feature's two client
implementations were correct, and the whole point of this capstone is that "correct separately" and
"correct together" are different claims. No part of the feature failed through a real surface; no
follow-up work item was filed.

## Wiring / how to re-run

* `internal/testfixtures/activitye2e/` — the shared fixture plane (Go, stdlib + `connect` only; no
  DB, no NATS). `cmd/activitye2e` serves it standalone on `:18080` with a control surface
  (`/__e2e/phase`, `/__e2e/stamp`, `/__e2e/end`, `/__e2e/reset`, `/__e2e/state`).
* `internal/tui/activity_e2e_pty_test.go` + `activity_e2e_states_test.go` — the TUI leg; gated on
  `ORCH_ACTIVITY_E2E=1` (+ `ORCH_PTY_SMOKE=1`), so the standing suite stays green.
* `frontend/tests/activity-line-e2e.spec.ts` + `frontend/playwright.activity-e2e.config.ts` +
  `frontend/vite.activity-e2e.config.ts` — the GUI leg. The Playwright config declares **both**
  servers as `webServer` entries, so one command runs the entire observation against a fresh plane
  on `:18080` / `:5174` and can never attach to a stale plane or the container's own sandbox plane.
* `scripts/activity-line-e2e.sh` — the combined runner.

---

# Addendum — PR Reviewer (step 3/5)

The reviewer independently re-ran both live legs and the standing suites on this branch, then
audited the harness against the work item's observation table. **Both legs pass as committed.** The
audit found two states in that table that the capstone never observed, and one pre-existing harness
fragility the added observation exposed. All three are fixed here, in this task (AC10: no deferred
edge).

## What the reviewer re-ran (all green, on this branch)

| Command | Result |
|---|---|
| `go build ./internal/testfixtures/activitye2e/...` | exit 0 |
| `go vet ./internal/testfixtures/activitye2e/... ./internal/tui/...` | clean |
| `ORCH_ACTIVITY_E2E=1 ORCH_PTY_SMOKE=1 … go test ./internal/tui -run 'TestActivityLineE2E$\|TestActivityLineE2ETurnEnd\|TestActivityLineE2EZeroToolCalls'` | **PASS** (124.3 s) |
| `cd frontend && npx playwright test --config playwright.activity-e2e.config.ts --project=dark-desktop` | **PASS** (2 tests, 55.3 s + 10.0 s) |
| `go test ./internal/tui/... ./internal/toolclass/... ./internal/testfixtures/...` | **ok**, all packages |
| `cd frontend && npx tsc -b` | exit 0 |
| `cd frontend && npx vitest run` | **796/796** (77 files) |
| `semgrep scan --config .orchicon/semgrep_orchicon.yml --error <changed files>` | **0 findings** |

## Gap 1 — the 35s re-dial band was never observed (fixed, both clients)

The table names *"Silence past re-dial (35s) → the re-dial line"* as its own state, distinct from the
25s warn band. The capstone asserted only the warn band, so an escalation that stopped at 25s — the
operator told "no output" but never that the stream is about to be re-dialled — would have passed.

Now observed live in the SAME stall, in both clients:

* TUI `TestActivityLineE2E/7_…` — `tui-10b-ac5-redial-band.txt`: `│Orchicon is weighing… · no output
  for 35s — the stream will re-attach if it stays silent│`, counters absent.
* GUI — `gui-04c-ac5-redial-band.png`: same band asserted on the rendered DOM.

Both also assert the re-dial line KEEPS the watchdog's own "no output for Ns" and DROPS the counter.

## Gap 2 — "turn ends: the line clears and the body gets its row back" was never observed (fixed)

The table's last row, and the state the whole feature must not break. The plane already shipped an
unused `Plane.EndTurn` / `POST /__e2e/end` — the control existed and nothing drove it.

* TUI `TestActivityLineE2ETurnEnd` (new, own session): `tui-14-turn-live-before-end.txt` (line up with
  a counter) → `plane.EndTurn()` → `tui-15-turn-end-line-cleared.txt` (no activity row on the frame,
  transcript witness still present).
* GUI `the line clears when the turn ends` (new): `gui-06-turn-live-before-end.png` →
  `endTurn()` → `gui-07-turn-end-line-cleared.png` (`[data-testid=ask-activity-line]` count 0,
  reply still visible).

## Defects found in the HARNESS and fixed here

These are harness bugs, not client bugs — the feature's two client implementations were not
touched. They are fixed in this task rather than filed (AC10).

1. **`EndTurn` poisoned every later turn.** `p.done` was closed once via a process-lifetime
   `sync.Once`, so after the first `EndTurn` every stream the plane opened returned immediately and
   the control surface worked exactly once — which is why no turn-end observation could coexist with
   the other legs. **Fix** (`plane.go`): `Reset` now replaces `p.done` and zeroes the `Once`; a
   stream pins the channel it watches at attach (`myDone`), so it never races the replacement.
   Pinned by `TestEndTurnIsUndoneByReset`.
2. **`Reset` left the SPA's auth routes down.** A leg that stopped the plane (`down`/`rpc-down`)
   flipped `Sessions.down` on, and a later reset left it on — the next leg's browser would load an
   unauthenticated page and observe nothing. **Fix**: reset clears it. Pinned by
   `TestResetReenablesAuthRoutes`.
3. **The GUI's restore-to-flight leg raced the 30s rolling window** (pre-existing, latent). It
   re-asserted `COUNTER_RE` after a stall whose length it did not bound; once the new 35s band
   extended that stall, the calls had honestly aged out of the summarizer's window and the assertion
   failed. The TUI leg already solved this with `plane.Reissue()`; the browser had no equivalent.
   **Fix**: `POST /__e2e/reissue` + `reissue()` in the spec, used before the kill leg — counters are
   re-earned with fresh work, never rewound. Pinned by `TestReissueLandsFreshCalls` and
   `TestControlSurfaceServesEndResetAndReissue`.

The fixture plane's control surface now has its own non-gated unit test (`plane_test.go`, 5 tests) so
the mechanics the opt-in E2E legs depend on are asserted in the standing suite.

## Verdict

All ten acceptance criteria were already met as committed; the two additions make the observation
table complete rather than leaving named states proven only by argument. Both live legs and every
standing suite are green on this branch.

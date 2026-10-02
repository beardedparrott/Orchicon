# Live run recipe — diff-surface end-to-end harnesses

The two harnesses committed by the terminal verification item (SSE step):

| harness | file | gate |
|---|---|---|
| TUI real-PTY gate | `internal/tui/diff_e2e_pty_test.go` (+ `diff_e2e_fixture_test.go`) | `ORCH_DIFF_E2E=1` |
| GUI Playwright spec | `frontend/tests/diff-surface-e2e.spec.ts` | env ids (below) |

Both **skip cleanly** without their inputs, so the standing suites stay green.

## 1. TUI real-PTY gate (self-contained — no plane needed)

The gate builds its own disposable plane in-process (`diffE2EPlane`), so it needs
only the built binary and a pty. From the repo root:

```sh
PROJ=/home/beardedparrott/projects/Orchicon   # the canonical project dir
mkdir -p /var/tmp/orchicon/home /var/tmp/orchicon/gocache /var/tmp/orchicon/gotmp
export HOME=/var/tmp/orchicon/home
export GOMODCACHE=$PROJ/.dev/mod GOCACHE=/var/tmp/orchicon/gocache GOTMPDIR=/var/tmp/orchicon/gotmp
make build                                     # emits bin/orch
ORCH_PTY_SMOKE=1 ORCH_DIFF_E2E=1 \
  ORCH_DIFF_E2E_OUT=/var/tmp/orchicon/frames \
  go test ./internal/tui -run TestDiffE2E -v -timeout 600s
```

`ORCH_PTY_SMOKE=1` is required from an interactive terminal (the pty gate would
otherwise be skipped to avoid hijacking the operator's own terminal). Each test
dumps the replayed screen grid to `$ORCH_DIFF_E2E_OUT` for the review to quote.

Tests:
- `TestDiffE2EAskMountPTY` — Ask mount at 80×24 and 200×50: pane renders the
  fixture ledger; the long line's tail token is reachable; a bar column paints;
  Tree-tab and Timeline-tab row clicks each focus the diff.
- `TestDiffE2EExecutionMountPTY` — Execution mount: pane renders the ledger.
- `TestDiffE2EResizeAndRestartPTY` — ctrl+right widens the DRAWN pane; a restart
  of the real binary (same config dir) redraws the same width.
- `TestDiffE2EHonestStatesPTY` — empty ledger renders the empty text; a failing
  fetch renders the error line, never the empty text.

**Why the fixture plane, not the running dev plane:** the TUI pane's ledger is
served over the real `FileEditService` RPC — the harness exercises the production
fetch path end to end. The `(owner_kind, owner_id)` tuple the pane sends is also
what the review's SQL quotes against the live plane (see §3).

## 2. GUI Playwright spec (live SPA + dev plane)

```sh
export E2E_ASK_CONV_ID=<conversation id with >=2 file-edit ledger rows>
export E2E_EXEC_ID=<execution id with >=2 file-edit ledger rows>
export E2E_FILE_A=<ask path A>   E2E_FILE_B=<ask path B>
export E2E_EXEC_FILE_A=<exec path A> E2E_EXEC_FILE_B=<exec path B>   # optional; defaults to the ask pair
export E2E_USERNAME=<seeded local account>  E2E_PASSWORD=<its password>   # optional; enables live login
PLAYWRIGHT_BASE_URL=http://localhost:5173 \
  npx playwright test tests/diff-surface-e2e.spec.ts
```

Start the dev plane (`orchicon serve` on the SPA's proxy target, port 8080 — see
`frontend/vite.config.ts`) and the SPA first. In the runtime container there is
**no pnpm**, so start Vite directly: `node_modules/.bin/vite --host 127.0.0.1
--port 5173` (the config's `webServer` needs pnpm and will exit 127; with the
server already up, `reuseExistingServer` lets the run proceed).

Two mounts hold **different** owner tuples, so give each its own file pair
(`E2E_EXEC_FILE_*`) or the Execution click-through asserts against an Ask path
and cannot find the row. The six projects in `playwright.config.ts` (dark/light
× desktop 1280×800 / tablet 768×1024 / mobile 375×812) supply the light+dark
scrollbar check and the sub-768px drawer. The main claims run against the REAL
ledger; only the two honest-state subtests intercept `GetSessionFileEdits` (an
empty `{edits,maxSeq}` body and a 500).

**Live-run gotchas (learned running this against a real plane):**
- The SPA keeps its access token **in memory only**. `E2E_USERNAME/PASSWORD`
  make the spec `POST /auth/local-login` and seed the token via the app's own
  `sessionStorage["orchicon_access_token"]` stash path (the OIDC-callback
  mechanism); that survives full-page navigations and the reload the resize
  check performs. Without credentials the spec reaches `/login`, never the rail.
- `waitForLoadState("networkidle")` never settles — the mounts poll
  (ListMessages / execution status) — so every navigation uses
  `domcontentloaded`.
- The rail is the inline `<aside>` at ≥768px but the `role="dialog"` overlay
  below 768px (no `<aside>` at all); the resize separator exists only inline.
  `diffRail()` in the spec scopes to whichever is present.
- A browser download may be needed: `npx playwright install chromium`
  (`PLAYWRIGHT_BROWSERS_PATH=/ms-playwright` in the container).


## 3. Quoting the live ledger tuples

For the acceptance review, capture the rows for the exact tuple each mount
resolved:

```sql
SELECT id, seq, path, tool
FROM file_edit_ledger
WHERE tenant_id = $TENANT
  AND owner_kind = 'ask_conversation' AND owner_id = $CONV_ID
ORDER BY seq;
-- and again with owner_kind = 'execution' AND owner_id = $EXEC_ID
```

Both mounts resolve the SAME tuple shape: TUI at `internal/tui/app.go:1236`
(`diffOwner`), GUI via `GetSessionFileEdits{owner_kind, owner_id, from_seq}`.

## 4. Regression pins (same pass)

```sh
go test ./internal/tui/... ./internal/fileedit/... ./internal/tui/diffs/...
ORCH_PTY_SMOKE=1 go test ./internal/tui -run TestPTY
cd frontend && npx tsc -b && npx vitest run      # there is NO `typecheck` script
make build                                        # emits bin/orch
```

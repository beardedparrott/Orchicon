# QA report — native (orchicon) Ask file-edit owner attribution

Branch: `native-orchicon-adapter-ask-...-tfgptvyzt97jgsy5`
Change under test: commit `43c3a355` (implementation) + `be9608fa` (QA real-service pins).

## Verdict: PASS — every acceptance criterion verified live; no bugs found.

### Root cause (confirmed with file:line proof)
The native (`orchicon`) bridge runs Ask tools **in-process** via
`NativeBridge.executeToolCalls` (`internal/orchicon/chatturn.go:1224`), not via a
Session/loop. That funnel had **no** file-edit hook, and the execution funnel
(`internal/orchicon/loop.go:1059`, always a real execution id) and the
`tool_use`-gated Ask hook (`internal/askorchicon/chat.go:2296`) are both
unreachable on this transport. The earlier version of this item passed because it
verified `internal/api/api.go:449`'s Ask hook — which does not run on the native
transport. The operator's diffs worked anyway because the **post-turn git sweep**
(`askorchicon/chat.go:1610` → `api.go:488` → `fileedit/reconcile.go`) backfilled
correct rows under the right owner with `tool=reconcile:git`. The fix adds a
dedicated Ask hook (`SetAskFileEditHook`) fired from `executeToolCalls`, attributed
`(ask_conversation, <convID>)`, with `execDir=""` (the engine payload is exact — no
repo needed). `newFileEditHook`'s owner kind became a parameter so one constructor
serves execution and Ask without forking.

## Acceptance criteria → evidence

1. **Tuple asserted (not a row count).** Unit tests assert the `(kind,id)` tuple
   both directions (`internal/orchicon/askfileedit_hook_test.go`,
   `internal/server/fileedit_hook_test.go:TestFileEditHook{Ask,Execution}OwnerTuple`).
   **QA added** `internal/server/fileedit_ask_attribution_db_test.go` (commit
   `be9608fa`): drives the PRODUCTION constructor with a **real PG store** and reads
   back through the **real fetch RPC** by `(owner_kind, owner_id)` — PASS.

2. **Delta-proof against the git sweep.** Unit test
   `TestAskLiveRowPrecedesTerminal` records hook-before-`idle` order and asserts
   `tool=write` (never `reconcile:git`). **Live:** the ledger row's `tool` is
   `write` and it is present before turn end; the git sweep would tag
   `reconcile:git`.

3. **Live, both clients, native adapter.** See "Live proof" below. GUI Diff/Tree/
   Timeline all render the edit; Timeline shows `write` (not `reconcile:git`).

4. **Non-git / no-project-dir case.** `TestAskLedgerWorksOutsideAGitRepo` + the QA
   DB test both use a non-git temp dir and an `execDir=""` hook call — the edit is
   ledgered there, where the sweep produces nothing (**live proven too**:
   project_dir `/tmp/orchicon/stubproj` is a bare `git init` with no changed paths
   committed; the row came from the live hook, `tool=write`).

5. **No execution regression.** `TestFileEditHookExecutionOwnerTuple` +
   DB test `TestExecutionLiveHookRowIsUnchangedByAskWiring` (row under
   `(execution,<execID>)`, absent from the Ask query). Live plane execution ledger
   (16 pre-existing `execution` rows) unchanged.

6. **No new orphans.** Live: `select ... where owner_id like 'orchicon-ask:%'`
   → **0 rows**; the synthetic `(execution,"orchicon-ask:<convID>")` query → `{}`.
   Historical orphans: **none exist** — the `(execution,"orchicon-ask:*")` tuple was
   never written by any code path (the prior Ask path only ever wrote correct rows
   via the sweep). No purge needed; stated explicitly.

7. **Both transports covered.** Native = `chatturn.executeToolCalls` hook
   (new). Opencode/legacy = `api.go:449` Ask hook invoked from
   `askorchicon/chat.go:2286`. Both resolve `db.FileEditOwnerAskConversation` +
   `convID`; the reconciler's owner (`api.go:493`) stays `ask_conversation` so
   sweep and live rows agree.

8. **Empty-vs-error honest.** `DiffTimeline` renders "No file edits for this
   session yet." only on `files.length===0`; `DiffSidebar` renders
   `LedgerErrorBanner` (`role=alert`, "Couldn't load file edits…") whenever the
   fetch/stream errors, above tab content — regression-pinned, unchanged by this diff.

## Live proof (my own sandbox plane, not the prod instance)
- Fixed binary built from the branch; own `orchicon serve` on **:8081** against the
  container Postgres (5432) / NATS (4222), `ORCHICON_SANDBOX_PLANE=1`.
- Stub OpenAI-compatible provider on :9099 (emits one `write` tool call).
- Project `proj_stub_qa` → `project_dir=/tmp/orchicon/stubproj` (non-git).
- Accept policy `/tmp/orchicon/stubproj/**` (native Ask write/edit are
  consent-gated — see FACTS).
- Conversation `modelRef=orchicon/stub/stub-model`, mode **ITERATION** (Brainstorm/
  QuickWork deny write/edit/batch_write/bash), driven via `ChatStream` (Connect
  streaming envelope).
- Result: file `ask_stub.txt` written; ledger row
  `(ask_conversation, <convID>, tool=write, real unified diff)`; client query
  `GetSessionFileEdits{ownerKind:"ask_conversation", ownerId:<convID>}` returns it;
  `(execution,"orchicon-ask:<convID>")` → `{}`; 0 orphan rows.
- GUI (:8080 SPA, real `/login` form): Diff/Tree/Timeline tabs render
  `ask_stub.txt  +1 −0`; Timeline tool column = `write`.

## Suites (all green)
`internal/orchicon`, `internal/server`, `internal/fileedit`, `internal/askorchicon`,
`internal/tui`, `internal/api`, `internal/scheduler` — PASS. The 2 scheduler
DB-backed tests that flake under the *leaked* `ORCHICON_TEST_DSN` (a worktree test
collision, no Postgres dependency in this change) pass in isolation and touch no
file in this diff; they are environmental, present at base.

## Notes / non-blocking
- Native Ask `write`/`edit` output IS an engine `file_edits` payload
  (`worktree.BatchWrite`), so the `execDir=""` hook records it — confirmed live.
- TUI uses the identical tuple I proved live (`internal/tui/app.go:1233`
  `return "ask_conversation", m.chatConvID`); the live TUI-PTY run was not executed
  (time-box) — the query path is the same one verified against the real service.

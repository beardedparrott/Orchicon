# TUI Diff Sidebar — slide-out pane consuming the shared pipeline

**Design authority:** `docs/tui-diff-sidebar-slide-out-pane-consuming-the-shared-pipeline.md`.
**Status:** plan delivered (workflow step 1 — Principal Software Architect). Not implemented.
**Git base:** implementation MUST be based on **`origin/develop`** (tip `f3ccfd0fe`), NOT
the current worktree HEAD (`95b45fd27`), because the worktree branch forks a stale `develop`
that contains **no** `internal/tui/` tree, **no** `lipgloss/bubbletea` in `go.mod`, and **no**
diff pipeline. See §0.

---

## 0. Grounding warning (READ FIRST)

The current worktree HEAD (`95b45fd27`, merge of PR #504) is a stale `develop`. It has:

- **No** `internal/tui/` package (the TUI shell).
- **No** `github.com/charmbracelet/{bubbletea,bubbles,lipgloss}` in `go.mod`.
- **No** diff pipeline (`internal/fileedit`, `FileEditService`), **no** GUI `DiffSidebar`,
  **no** shared fixtures.

All of that exists only on **`origin/develop`** (`f3ccfd0fe` = merge of PR #512
`gui-diff-sidebar` + PR #507 `diff-pipeline`). Evidence:

- `git branch -a --contains c91486b06` → `remotes/origin/develop` (GUI sidebar commit).
- `git branch -a --contains 06accb91a` → `remotes/origin/develop` (PR #507 merge).
- `git ls-tree -r origin/develop --name-only | grep '^internal/tui/'` → 43 files.
- `git show origin/develop:go.mod | grep lipgloss` → lists bubbletea/bubbles/lipgloss.

> **DECISION (authoritative):** the SSE must create the feature branch **off
> `origin/develop`** (rebase onto `f3ccfd0fe`), and the whole plan below is grounded against
> `origin/develop`. Everything referenced by `path:line` is a path on `origin/develop`, with
> line numbers as they appear there.

---

## 1. What the shared pipeline is

The diff pipeline is the **durable file-edit ledger**. One row = one ground-truth,
server-computed unified diff for one path touched by one file-editing tool event of one
owner (an execution or an Ask conversation). Diffs come from real file-state snapshot pairs
(never parsed from tool prose) and reconcile against worktree git state on completion.

### 1.1 Proto contracts (path on `origin/develop`)

`proto/orchicon/api/v1/file_edit.proto` defines `FileEdit`:

| field | go getter | meaning |
|---|---|---|
| `id` | `GetId()` | ledger row id; doubles as stream `event_id` |
| `owner_kind` | `GetOwnerKind()` | `execution` \| `ask_conversation` |
| `owner_id` | `GetOwnerId()` | execution id or Ask conversation id |
| `seq` | `GetSeq()` | monotonic per owner; doubles as stream `sequence` |
| `path` | `GetPath()` | slash-separated, worktree-relative |
| `kind` | `GetKind()` | `create` \| `modify` \| `delete` |
| `unified_diff` | `GetUnifiedDiff()` | the unified diff text (empty for binary/oversized) |
| `before_size` / `after_size` | `GetBeforeSize()` / `GetAfterSize()` | byte counts |
| `tool` | `GetTool()` | `batch_write` \| `write` \| `edit` \| `opencode:*` \| `reconcile:git` |
| `is_binary` / `truncated` / `git_confirmed` | bool getters | renderer flags |
| `created_at` | `GetCreatedAt()` | `*timestamppb.Timestamp` |

`proto/orchicon/api/v1/file_edit_service.proto` defines the two RPCs the TUI consumes:

- `rpc GetSessionFileEdits(GetSessionFileEditsRequest) returns (GetSessionFileEditsResponse)` —
  request has `tenant_id`, `owner_kind`, `owner_id`, `optional int64 from_seq`; response has
  `repeated FileEdit edits` + `int64 max_seq` (the resume point).
- `rpc StreamFileEdits(StreamFileEditsRequest) returns (stream StreamFileEditsResponse)` —
  request has `tenant_id`, `owner_kind`, `owner_id`, `optional int64 from_sequence`; response
  has `FileEdit event`, `string event_id`, `int64 sequence`.

### 1.2 Server-side truth

`internal/fileedit/diff.go` — `ComputeUnifiedDiff(before, after []byte, path string) DiffResult`
is the single engine. **Myers** edit script (same algorithm git uses), 3 context lines,
paired hunk line numbers, `\ No newline at end of file` markers. Output is LF-terminated,
UTF-8 passthrough (line-level diffing only). Binary = NUL in first 8 KiB.

### 1.3 The GUI twin (the renderer the TUI must match)

`frontend/src/components/diffs/DiffSidebar.tsx` — **the TUI's sibling**. Three tabs
(`diff | tree | timeline`), open/tab/selected state owned by the HOST and persisted
(`usePersistentState`), data from `useSessionFileEdits` (merge of `GetSessionFileEdits` +
`StreamFileEdits` via `mergeEdits`).

The pure renderer it calls into is `frontend/src/lib/diff/sideBySide.ts`:

- `parseUnifiedDiff(unifiedDiff: string): SideBySideRow[]` — turns a stored `unified_diff`
  into paired old|new rows with `lineNoOld`/`lineNoNew` (1-based), `sign`, `oldText`/`newText`,
  `kind` (`add|del|ctx`), and optional `oldSpans`/`newSpans` (word-level emphasis).
- `emphasizeTokens(oldText, newText)` — trims common prefix/suffix, flags changed middle as a
  single span (or a whole-middle span if the changed length > `MAX_EMPHASIS=256`).
- `groupByFile(edits)` — flattens a chronological edit list into per-file groups carrying
  `adds`/`dels` tallied from the **latest** edit's `unified_diff`, `kind`, `lastTool`, `lastAt`.
- `isLanguage(path)` / `languageFor(path)` — extension → language map.

**`SideBySideRow` shape (the cross-language contract to mirror in Go):**

```ts
export interface SideBySideRow {
  lineNoOld: number | null;
  lineNoNew: number | null;
  sign: "+" | "-" | " " | "";
  oldText: string;
  newText: string;
  kind: "add" | "del" | "ctx" | "hunk";
  oldSpans?: EmphasisSpan[];
  newSpans?: EmphasisSpan[];
}
export interface EmphasisSpan { start: number; end: number; type: "add" | "del"; }
```

`frontend/src/components/diffs/DiffView.tsx` renders `parseUnifiedDiff` output as **two
columns** (old | new) with paired line numbers, line-level red/green via Tailwind tokens, and
word-level emphasis via `renderLine`. It falls back to a **unified** single-column mode when
`unified` is true (the GUI forces this on narrow screens via `MOBILE_BREAKPOINT=768`).
`DiffTree.tsx` / `DiffTimeline.tsx` render the tree/timeline tabs.

### 1.4 The three-way consistency contract

The **same** shared fixtures drive all three renderers/tests:

- `internal/testfixtures/fileedit/*.json` (13 vectors: append, binary, create, delete,
  identical, modify, multihunk, no-newline, noop, truncate, unicode, …).
- Go engine: `internal/testfixtures/fileedit.go` + `internal/fileedit/diff_test.go`.
- TS twin: `frontend/src/lib/fileedit/testvectors.test.ts`.
- TS renderer: `frontend/src/lib/diff/sideBySide.test.ts` (globs the same fixtures, runs
  `parseUnifiedDiff` over each vector's `expected_unified_diff`).

**The TUI's renderer tests MUST also consume these same fixtures** so the "byte-for-byte with
the TS renderer's logical output" acceptance criterion is testable in the Go test suite. The
Go test reads `internal/testfixtures/fileedit/*.json` — same vector set, same
`expected_unified_diff`, and asserts the same rows (line numbers, kind, sign, spans) that
`sideBySide.test.ts` asserts.

Fixture shape (from `internal/testfixtures/fileedit/modify.json`):

```json
{ "name":"modify-two-lines", "path":"docs/x.md",
  "before":"line1\nline2\nline3\n", "after":"line1\nline2x\nline3\n",
  "expected_kind":"modify",
  "expected_unified_diff":"--- a/docs/x.md\n+++ b/docs/x.md\n@@ -1,3 +1,3 @@\n line1\n-line2\n+line2x\n line3\n",
  "expected_binary":false, "expected_truncated":false }
```

---

## 2. TUI integration points (path on `origin/develop`)

### 2.1 Shell

`internal/tui/app.go` — the `App` model (six-tab shell). All screens embed `screenkit.Base`.
The chat dock (`internal/tui/dock/dock.go`) is always present and composes below every screen.

- `NewApp(cl, profile, serverVersion)` (app.go:110) builds the shell.
- Factories map `m.factories` (app.go:143-153) lazily construct screens; they pass `tenantID=""`
  because the plane resolves the tenant from the bearer for *stream* RPCs.
- `m.dock`, `m.chatFocus`, `m.screens`, `m.execSessions`, `m.chatConvID` are the state the
  sidebar toggling must integrate with.
- `View()` (app.go:270) composes `tabBarView + screen.View() + dock.View() + footer.View()`.
- `dispatch(msg)` (router.go) routes keyboard + mouse. `Focus`/`Blur` via `setFocus`.
- `contentHeight()` (app.go:216) = `height - 3 - dock.Lines()`.

The GUI keeps the sidebar **open state at the HOST and persists it across navigation**. The
TUI analogue: the **App holds the sidebar state** (open + tab + selectedPath) so it survives
tab switches, and `SwitchTo`/`Close` must NOT reset it. This satisfies "Sidebar open state
persists while navigating between screens."

### 2.2 Screen contract

`internal/tui/screens/screenkit/base.go`:

- `Screen` interface: `Init() tea.Cmd; Update(tea.Msg); View() string; Name(); SetSize(w,h);
  Close()`.
- `Base.SetSize(w,h)` (base.go) lays out equal-width source panes + one detail pane.
- `Base.Update` (base.go:200) handles keys (`up/down/left/right/f/esc/enter/tab`) and
  `tea.MouseMsg` (base.go:269). Mouse `Click`/`Wheel`/`ClickDetail` (helpers.go:22-34).
- `Base.mouse` (base.go:284) deliberately returns `nil` for `Motion`/`Release` so `Shift+drag`
  stays native text selection.
- `Detail` (kit.go) is the scrollable key-value pane; `Detail.SetContent(title, fields, body)`.

The execution screen (`internal/tui/screens/execution/screen.go`) and ask screen
(`internal/tui/screens/ask/screen.go`) are the two hosts; they carry `DetailID()` and, for
execution, `SessionEvents(execID)` / `RunningExecutionID()`.

### 2.3 Stream engine (mirror of `useStream.ts`)

`internal/tui/stream/stream.go` — `Sub[Resp]` with `Open`/`GetEventID`/`GetSequence`/`Filter`/
`OnEvent`/`OnStatus`, reconnect + exponential backoff, dedup by event id, resume from last
sequence, drop-oldest ring (default 200). This is exactly what the TUI must use for
`StreamFileEdits`.

`internal/tui/subs/subs.go` — `Registry` wires `stream.Sub` to bubbletea via status channels +
event-poke channels; `ExecutionEvents`, `ProjectEvents`, etc. are the existing templates. The
new **`FileEdits`** subscription follows the same shape (but see §4.1 — it needs a non-empty
tenant).

### 2.4 Client set

`internal/tui/client/client.go` — `Clients` struct. **It does not currently have a
`FileEdits` client.** The generated one exists at
`api/gen/go/orchicon/api/v1/apiv1connect/file_edit_service.connect.go` — `FileEditServiceClient`
interface with `GetSessionFileEdits` and `StreamFileEdits` (constructor
`NewFileEditServiceClient`). Add `FileEdits apiv1connect.FileEditServiceClient` to `Clients` and
construct it in `NewWithHTTPClient` (client.go) exactly like the others.

---

## 3. Feature scope and the plan's shape

New package `internal/tui/diffs` (sibling of the existing `internal/tui/{dock,chat,stream,subs}`).

| file (new) | contents |
|---|---|
| `internal/tui/diffs/parse.go` | `ParseUnifiedDiff(string) []Row` — pure port of `parseUnifiedDiff`; **the byte-for-byte contract**. |
| `internal/tui/diffs/rows.go` | `Row` type (`lineNoOld, lineNoNew int` with `Has*` bools, `sign`, `oldText`, `newText`, `kind`, `oldSpans, newSpans`), `EmphasisSpan`. |
| `internal/tui/diffs/emphasis.go` | `EmphasizeTokens(old, new)` — port of `emphasizeTokens` (+ `MAX_EMPHASIS=256`). |
| `internal/tui/diffs/group.go` | `GroupByFile([]FileEdit) []FileGroup` — port of `groupByFile`; `FileGroup{Path, Kind, Edits, Adds, Dels, LastTool, LastAt}`. |
| `internal/tui/diffs/render.go` | `RenderRow` + `RenderPane` → lipgloss-styled frames (side-by-side + unified narrow fallback + color profile degradation). Uses `internal/tui/theme`. |
| `internal/tui/diffs/model.go` | bubbletea `Model` for the pane: open/tab/selectedPath, scroll offset, `Init/Update/View/SetSize/Close`, mouse wheel/click, `y` copy binding. |
| `internal/tui/diffs/store.go` | `Store` — the Connect RPC layer: `GetSessionFileEdits` (durable fetch) + `StreamFileEdits` (live via `stream.Sub`), merged with `mergeEdits` (port of `mergeEdits`). Owns the tenant-id discovery + cache. |
| `internal/tui/diffs/parse_test.go`, `emphasis_test.go`, `group_test.go` | Go tests over the **shared fixtures** (`internal/testfixtures/fileedit/*.json`). |
| `internal/tui/diffs/tenant.go` | `ResolveTenantID(ctx)` — cache of `Auth.ListIdentities` → `Identity.tenant_id`. |

### 3.1 Shared fixture consumption in Go tests

`parse_test.go` will `go:embed` (or read at runtime via `os.ReadFile` — the existing Go tests
use `internal/testfixtures/fileedit.go` which provides an embed/read) the same
`internal/testfixtures/fileedit/*.json` vectors, then assert, per vector:

- `ParseUnifiedDiff(expected_unified_diff)` yields the same row count, and for each row the same
  `lineNoOld`/`lineNoNew`/`sign`/`kind` as the TS `sideBySide.test.ts` expectations.
- Round-trip: rejoining `sign+text` reproduces every content line (the TS test's
  `rebuilt.includes(line)` assertion).
- The `no-trailing-newline` vector annotates `⟪no newline⟫` onto the preceding add/del row and
  emits no phantom ctx row.
- `EmphasizeTokens` returns the same span ranges as the TS test (e.g. `The quick ` prefix /
  ` fox` suffix not spanned).
- `GroupByFile` tallies `adds`/`dels` from the **latest** edit (create vector → +3/0, modify
  vector → +1/−1).

**Note on fixture reader:** `internal/testfixtures/fileedit.go` is the existing loader (paths
under `internal/testfixtures/`). The Go test in `internal/tui/diffs` can import it directly
(`github.com/beardedparrott/orchicon/internal/testfixtures`); this is the same package the Go
engine's `diff_test.go` uses, so there is a single fixture source.

---

## 4. Data flow (the "consuming the shared pipeline" contract)

The TUI pane consumes the **exact same** two RPCs as the GUI `useSessionFileEdits`:

1. **Durable fetch** `GetSessionFileEdits` → initial + catch-up list and `max_seq`.
2. **Live stream** `StreamFileEdits` → new entries as persisted, via `stream.Sub`
   (reconnect/resume/dedup identical to `useStream.ts`).
3. **Merge** with `mergeEdits`: durable is a superset up to ~2 s ago; live events append only
   when their `seq > durable max_seq` and dedup by `id` (`event_id == ledger row id`).

`isLive` flag: a running execution/ask = live (fetch + stream + merge); a completed run =
durable, **git-reconciled** ledger only (`GetSessionFileEdits`).

### 4.1 Tenant id (THE CRITICAL WIRING DIFFERENCE)

Unlike `StreamProjectEvents`/`StreamExecutionEvents` (which ignore `req.TenantId` and let the
plane resolve it from the bearer), **`FileEditService` REJECTS an empty tenant**:

`internal/fileedit/rpc.go`:
```go
tenantID := req.Msg.GetTenantId()
if strings.TrimSpace(tenantID) == "" {
    return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("tenant_id must not be empty"))
}
```
(and the same check in `StreamFileEdits`.)

The GUI solves this with `sessionTenantId()` = `useSessionStore.getState().session.tenant_id`.
The TUI has no session store and its `Profile` has no tenant field
(`internal/tui/config/config.go` — `Profile{Name, URL, AuthMethod, Token, Username,
InsecureSkipVerify, Newline}`).

**How the TUI resolves its tenant:** `Auth.ListIdentities` resolves the tenant from the bearer
credential (`internal/auth/service.go` `ListIdentities` uses `tenant.FromContext(ctx)`) and
returns `Identity` rows whose proto field 2 is `tenant_id`. So a one-shot unary call to
`cl.Auth.ListIdentities{PageSize:1}` (exactly what `probeIdentity` in `cmd/orch/main.go:233`
already does for the footer) gives the tenant id. Cache it on first use.

> **DECISION (revisitable):** resolve tenant via `ListIdentities` (needs no identity id, unlike
> `GetIdentity` which requires `req.Msg.Id`). Store in the `diffs.Store` and pass to every
> FileEdit RPC. This mirrors the existing footer `probeIdentity` call and avoids adding a tenant
> field to the persisted `Profile` (which would be a config-format change).

---

## 5. Rendering + mouse/keyboard (the acceptance-criteria-bearing surface)

### 5.1 Side-by-side renderer

`RenderPane(rows []Row, width int, profile termenv.Profile) string`:

- Two columns: old | new, each with a line-number gutter, the line text, and line-level
  red/green. Paired `lineNoOld`/`lineNoNew` (show blank when a side has no line — added lines
  have no old number, deleted lines have no new number).
- Line classification → lipgloss style: `add` → green, `del` → red, `ctx` → dim text.
- Word-level emphasis: `EmphasizeTokens` spans wrapped in a **reverse/underline attribute** on
  the changed middle (cheap, no syntax highlighter in deps — mirrors the GUI's "no highlighter"
  decision).
- Hunk-boundary rows: `kind` from parsing is `add/del/ctx`; the `hunk` kind is never a rendered
  row (matching the GUI, which tracks state but emits no hunk row).

### 5.2 Wide diffs / narrow pane

Mirror the GUI decision exactly (DiffView `unified` prop + `MOBILE_BREAKPOINT=768`):

- If the pane width ≥ a **min side-by-side width** (the GUI uses min-w-640px for two columns →
  ~64 cols at 10px/col; adapt to ~64 terminal cells), render side-by-side.
- If the pane width is below that (narrow window or the pane is taking too much horizontal
  space), **collapse to unified** single-column (sign + old-lineno + new-lineno + text).
- The TUI has no horizontal scroll of its own in the renderer — it uses the same "collapse to
  unified" strategy the GUI uses, so "mirror the GUI's decision for consistency" is satisfied.

**Pane width vs main-content reflow:** The sidebar is a left rail of fixed width
(`diffPaneWidth`, e.g. 48 cells) that slides out over/beside the content when open. When open,
the **main content area + bottom chat dock reflow** (content width shrinks by the pane width).
The shell must subtract the pane width from the screen's `SetSize` width and from the dock width
(see §6 wiring).

### 5.3 Mouse support

- **Wheel-scroll the diff**: `tea.MouseMsg` with `WheelUp`/`WheelDown` scrolls the pane's
  viewport. Follow the `screenkit.Base` pattern: `Motion`/`Release` return nil (Shift+drag stays
  native selection).
- **Click files/tabs**: left-button click on a tree/timeline file row selects it (→ opens its
  diff); click on a tab header switches tab; click on the toggle area toggles open/close.
- **Copy binding**: `y` on a selected file's diff copies the **unified diff** via **OSC 52**
  (`\x1b]52;c;<base64>\x1b\\`) so it works over SSH. The renderer must keep the raw unified diff
  string reachable (the `Store` keeps `unified_diff` on each edit) so `y` can copy the exact
  `unified_diff` text, not a re-rendered approximation.

### 5.4 Keyboard

- Toggle: `D` / `Shift+D` **outside text input** — the dispatch layer must swallow these ONLY
  when `m.chatFocus == focusContent` (i.e. not while the composer textarea is focused), so
  typing `d` in the dock is unaffected. **`Ctrl+D` is never used** (EOF muscle memory).
- `Esc` or re-toggle closes; `Esc` inside the pane also closes, and layout must be restored
  exactly (previous content/dock widths).
- `h/j/k/l`/arrows scroll within the diff + navigate the tree/timeline when the pane is focused;
  `y` copies the selected file's unified diff.

---

## 6. Wiring plan (where each piece registers)

| # | change | file | insertion point |
|---|---|---|---|
| 6.1 | Add `FileEdits apiv1connect.FileEditServiceClient` to `Clients` | `internal/tui/client/client.go` | after `Telemetry` field in `Clients` struct (client.go ~line 82); construct in `NewWithHTTPClient` after `c.Telemetry = ...` (client.go ~line 148) using `newClient(apiv1connect.NewFileEditServiceClient, ...)`. |
| 6.2 | Add `FileEdits(...)` subscription helper to the registry | `internal/tui/subs/subs.go` | new method modeled on `ExecutionEvents` (subs.go ~198). Takes `tenantID string` (non-empty), sets `Open` to call `cl.FileEdits.StreamFileEdits` with `TenantId: tenantID`, `GetEventID: m.GetEvent().GetId()` (ledger row id), `GetSequence: m.GetSequence()`, and `OnEvent` → `r.pokeEvent(name)`. |
| 6.3 | New package `internal/tui/diffs/` (parse/rows/emphasis/group/render/model/store/tenant) | `internal/tui/diffs/*.go` | new files. |
| 6.4 | Shell owns sidebar open/tab/selected state; toggle handling in `dispatch` | `internal/tui/app.go` + `internal/tui/router.go` | add fields `diffOpen bool`, `diffTab`, `diffPath` to `App` struct (app.go ~72); add a `KeyRoute` in `GlobalKeyRoutes` (router.go) for `d`/`shift+d` that only activates when `m.chatFocus == focusContent`; the `Handle` flips `m.diffOpen`, swaps in the pane. |
| 6.5 | Reflow: subtract pane width from screen + dock when open | `internal/tui/app.go` | in `contentHeight()` (app.go:216) and wherever `s.SetSize(w, m.contentHeight())` is called (dispatch WindowSizeMsg, router.go) — pass `w - paneWidth` to the screen when `m.diffOpen`; also shrink `m.dock.Width`. |
| 6.6 | App `View()` renders the pane as a left rail | `internal/tui/app.go` | in `View()` (app.go:270) — when `m.diffOpen`, write `diffs.RenderPane(...)` before `s.View()` and reserve its columns. |
| 6.7 | `Esc`/close restores layout; state persists across `SwitchTo` | `internal/tui/app.go` | `SwitchTo` (app.go:180) must not touch `m.diffOpen`/`m.diffTab`/`m.diffPath`; `Esc` route in `dispatch` sets `m.diffOpen=false` and re-derives widths. |
| 6.8 | Wire tenant resolution into the diffs `Store` | `internal/tui/diffs/store.go` + `internal/tui/diffs/tenant.go` | `Store` calls `cl.Auth.ListIdentities` (once, cached) to get `tenant_id`, then passes it to `GetSessionFileEdits`/`StreamFileEdits`. |
| 6.9 | Owner selection (which session's edits the pane shows) | `internal/tui/app.go` | derive from the active context: if `m.active==TabExecution` and the detail shows an execution → owner_kind `execution`, owner_id = `DetailID()`; if `m.active==TabAsk` → owner_kind `ask_conversation`, owner_id = `m.chatConvID`. Use the existing `activeContext()` (app.go) + `m.chatConvID`. |
| 6.10 | Registry: ensure FileEdits sub is closed on screen close | `internal/tui/subs/subs.go` + | the pane's `Close()` (via `screenkit.Base.Close`/tab switch) calls `m.reg.CloseAll()` which currently closes all registered subs; since the pane's sub is added to the same `reg`, it closes automatically — only need to add it via a new `r.FileEdits(...)` call from the model's `EnsureSubscriptions`/`Init`. |

---

## 7. Color degradation

Lipgloss/termenv degrades automatically; the existing theme already declares all colors as
`lipgloss.AdaptiveColor` (`internal/tui/theme/theme.go`) and `theme_test.go` has
`TestProfileDegradation` asserting SGR output under `termenv.TrueColor`, `ANSI256`, and `ANSI`
(16-color) profiles, plus an ASCII black/white path.

Add new styles in `internal/tui/theme/theme.go` (the single source of styling — every feature
imports theme, nothing else builds styles):

- `DiffAdd` = `Foreground(OK).Background(...)` (green line).
- `DiffDel` = `Foreground(Err).Background(...)` (red line).
- `DiffEmphasis` = a `Reverse(true)` (or underlined) style for word-level emphasis spans.
- `DiffHunk` = dim header for the `@@ … @@` line.
- `DiffLineNoOld`/`DiffLineNoNew` = dim line-number columns.

**Verify no unreadable output at each profile:** extend `theme_test.go`'s `TestProfileDegradation`
(or add a `diffs/degradation_test.go`) to render `RenderPane(...)` under each `termenv.Profile`
and assert: (a) `add`/`del` lines still carry a distinct SGR color code vs `ctx` in TrueColor and
ANSI256; (b) in the ANSI/16-color profile the `add`/`del` lines map to distinguishable
foreground/attributes (green/red) and never collapse to plain `ctx`; (c) ASCII path never emits
raw color codes. This is the "truecolor → 256 → 16, verify no unreadable output" criterion.

---

## 8. OSC 52 copy binding (works over SSH)

The `y` copy must emit the OSC 52 sequence `\x1b]52;c;<base64(unified_diff)>\x1b\\`. Bubbletea
renders the string returned by `View()` to the terminal; appending the OSC 52 sequence into the
rendered output is sufficient for terminals that support the clipboard set-selection. The copy
target is the **selected file's `unified_diff`** (kept verbatim on the `FileGroup`/`Store`), not
a re-render. Add a `copyBuf` field on the pane model: `y` stores the base64 payload and the next
`View()` emits it once (a `copyPending` flag); document that not all terminals honor OSC 52
(over SSH with tmux/screen, the outer terminal usually does).

---

## 9. Out of scope (explicit)

Editing, staging, or committing from the pane. View-only in v1. No client-side diff
computation from tool output — the TUI only consumes the ledger's `unified_diff`.

---

## 10. Numbered implementation list (SSE follows; zero blocking questions)

Base **`origin/develop`** (`f3ccfd0fe`). Create branch off it. Work only in `internal/tui/` +
`docs/`. Every step is independently compilable/testable.

1. **`internal/tui/client/client.go`**: add `FileEdits apiv1connect.FileEditServiceClient` to the
   `Clients` struct and construct it in `NewWithHTTPClient` with
   `newClient(apiv1connect.NewFileEditServiceClient, httpClient, base, opts2)`. The generated
   client is at `api/gen/go/orchicon/api/v1/apiv1connect/file_edit_service.connect.go`
   (`FileEditServiceClient` interface: `GetSessionFileEdits`, `StreamFileEdits`).

2. **`internal/tui/diffs/rows.go`**: define `type Row struct { LineNoOld, LineNoNew int;
   HasOld, HasNew bool; Sign string; OldText, NewText string; Kind Kind; OldSpans, NewSpans
   []EmphasisSpan }`, `type Kind string` (`KindAdd/KindDel/KindCtx/KindHunk`), `type EmphasisSpan
   struct{ Start, End int; Type string }`. This is the Go mirror of `SideBySideRow`.

3. **`internal/tui/diffs/parse.go`**: `func ParseUnifiedDiff(unified string) []Row` — faithful
   port of `parseUnifiedDiff`: split on `\n`, drop trailing empty element, track `oldLine`/
   `newLine` per hunk header (`@@ -old[,c] +new[,c] @@`), skip `---`/`+++`/`@@`, skip
   `\ No newline at end of file` (append `⟪no newline⟫` to the preceding row's add/del text),
   emit one row per `+`/`-`/` ` line. Set `HasOld`/`HasNew` so null line numbers are representable.
   Run `EmphasizeTokens` over adjacent del→add pairs at the end.

4. **`internal/tui/diffs/emphasis.go`**: `func EmphasizeTokens(oldText, newText string)
   (oldSpans, newSpans []EmphasisSpan)` — port of `emphasizeTokens`: trim common prefix & suffix
   (preserving graph), `MAX_EMPHASIS=256`; beyond it the whole changed middle is one span.

5. **`internal/tui/diffs/group.go`**: `type FileGroup struct { Path, Kind string; Edits
   []*apiv1.FileEdit; Adds, Dels int; LastTool string; LastAt int64 }` and `func GroupByFile(edits
   []*apiv1.FileEdit) []FileGroup` — port of `groupByFile`: group by `Path`, `kind`/`lastTool`
   from the newest edit, `LastAt` from `createdAt.seconds`, and `Adds`/`Dels` tallied by running
   `ParseUnifiedDiff` over the **latest** edit's `unified_diff`.

6. **`internal/tui/diffs/parse_test.go`** (PASS FIRST — pin the cross-language contract): load
   `internal/testfixtures/fileedit/*.json` via `internal/testfixtures` (same loader the Go
   engine's `diff_test.go` uses). For each vector with a non-empty `expected_unified_diff`:
   (a) `ParseUnifiedDiff` returns >0 rows; (b) row `kind`/`lineNoOld`/`lineNoNew` match the TS
   `sideBySide.test.ts` expectations for `modify-two-lines` (ctx 1/1, del old=2/new=null, add
   old=null/new=2), `create-new-file` (3 adds), `delete-file` (2 dels), and `no-trailing-newline`
   (exactly 1 ctx + 1 del + 1 add, del.oldText contains `⟪no newline⟫`, add.newText contains
   `⟪no newline⟫`, del.lineNoOld=2, add.lineNoNew=2); (c) round-trip: joining `sign+text`
   over the rows reproduces every content line of the vector's diff.

7. **`internal/tui/diffs/emphasis_test.go`**: assert `EmphasizeTokens("The quick brown fox",
   "The quick red fox")` returns one `del` span on old (`start=10,end=15`) and one `add` span on
   new (`start=10,end=13`); `("line2","line2x")` → 0 old spans, 1 new span; `("same","same")` → 0/0;
   300-char fully-different → both lengths >0.

8. **`internal/tui/diffs/group_test.go`**: over `create-new-file` + `modify-two-lines` fixtures,
   `GroupByFile` returns 2 groups; create group `Adds=3, Dels=0`; modify group `Adds=1, Dels=1`.

9. **`internal/tui/diffs/tenant.go`**: `func (s *Store) resolveTenantID(ctx) (string, error)` —
   calls `cl.Auth.ListIdentities(ctx, &apiv1.ListIdentitiesRequest{PageSize:1})` once, caches
   `resp.Msg.GetIdentities()[0].GetTenantId()`. Mirrors `probeIdentity` (cmd/orch/main.go:233).

10. **`internal/tui/diffs/store.go`**: `type Store struct{ cl *client.Clients; tenant string;
    live []*apiv1.FileEdit; byID map[string]bool; maxDurableSeq int64 }`. `func (s *Store)
    Fetch(ctx, ownerKind, ownerID string) ([]*apiv1.FileEdit, int64, error)` calls
    `cl.FileEdits.GetSessionFileEdits` with `TenantId: s.tenant, OwnerKind, OwnerId`; `func (s
    *Store) Live(ownerKind, ownerID string, isLive bool)` builds/updates a `stream.Sub` for
    `StreamFileEdits` (via a `r.FileEdits(...)` registry helper). `func MergeEdits(durable,
    live []*apiv1.FileEdit) []*apiv1.FileEdit` ports `mergeEdits`: dedup by `GetId()`, append
    only when `GetSeq() > maxDurableSeq`, stable sort ascending by `GetSeq()`.

11. **`internal/tui/subs/subs.go`**: add `func (r *Registry) FileEdits(cl *client.Clients,
    tenantID string, ownerKind, ownerID string) *stream.Sub[*apiv1.StreamFileEditsResponse]` —
    modeled on `ExecutionEvents` (subs.go:198). `Open` sets `TenantId: tenantID`,
    `OwnerKind`, `OwnerId`, and `FromSequence` when `fromSequence>0`; `GetEventID` =
    `m.GetEvent().GetId()`; `GetSequence` = `m.GetSequence()`; `GetEventID`/`GetSequence`/dedup
    identical to the stream engine. `OnEvent` → `r.pokeEvent(name)`.

12. **`internal/tui/diffs/render.go`**: `func RenderPane(rows []Row, width int, profile
    termenv.Profile) string` and helpers `renderSideBySide`, `renderUnified`. Use
    `internal/tui/theme.DiffAdd/DiffDel/DiffEmphasis/DiffHunk/DiffLineNoOld/DiffLineNoNew`.
    If `width < minSideBySideWidth` (≈64), collapse to unified. Handle `⟪no newline⟫` text,
    truncate long lines, pad so paired gutters align.

13. **`internal/tui/diffs/model.go`**: bubbletea `Model` embedding the `Store` + a scroll
    viewport. `Init/Update/View/SetSize/Close`. `Update` handles `tea.KeyMsg` (`j/k/up/down`
    scroll, `h/l` switch tab, `enter` focus, `esc` close, `y` copy) and `tea.MouseMsg`
    (`WheelUp/Down`, left-click on tabs/files). Use a `viewport.Model` (bubbles) for the diff
    scroll. `Close()` closes the `stream.Sub`.

14. **`internal/tui/theme/theme.go`**: add `DiffAdd`, `DiffDel`, `DiffEmphasis`,
    `DiffHunk`, `DiffLineNoOld`, `DiffLineNoNew` styles, all `AdaptiveColor`, plus a
    degradation test (extend `theme_test.go` `TestProfileDegradation`, or add
    `diffs/degradation_test.go`) asserting distinct SGR per profile.

15. **`internal/tui/router.go`**: add a `KeyRoute` for `d` and `shift+d` in `GlobalKeyRoutes`
    (only when `m.chatFocus == focusContent`; never `ctrl+d`). `Handle` flips `m.diffOpen`.
    Add an `esc` handler that closes the pane when it is open (and is NOT the composer's
    `esc`). Add `y` (copy unified diff via OSC 52) activated when the diff pane is open + a file
    selected.

16. **`internal/tui/app.go`**: add `diffOpen bool`, `diffTab diffs.Tab`, `diffPath string`,
    `diffStore *diffs.Store` to `App`. In `NewApp`, init the `diffStore` over `cl`. In `View()`
    (app.go:270), when `m.diffOpen` write the pane rail before the screen and reserve its width.
    In `contentHeight()` and the `WindowSizeMsg` handling (router.go `dispatch`), subtract
    `diffPaneWidth` from the screen width and `m.dock.Width` when open. In `SwitchTo`
    (app.go:180), do NOT reset `diffOpen/diffTab/diffPath`. Derive owner in a new
    `diffOwner()` method from `activeContext()` + `m.chatConvID`.

17. **`internal/tui/screens/execution/screen.go`** + **`internal/tui/screens/ask/screen.go`**:
    expose the current detail's owner id as an interface (`OwnerID() string`) or reuse
    `DetailID()`/`m.chatConvID` so the app can pick the right owner. (Minimal: reuse existing
    `DetailID()`; no signature change needed.)

18. **Wiring the live event poke**: in the diffs `Model.Update`, on `subs.EventPokeMsg`, re-run
    `MergeEdits(durable, live)` and repaint. Ensure the pane's `stream.Sub` is registered in
    `m.reg` so tab switch (`Screen.Close` → `reg.CloseAll`) tears it down; add the pane's sub
    the same way the execution screen adds `ExecutionEvents`.

19. **Manual/QA gate** (per acceptance criteria): `go test ./internal/tui/diffs/...`,
    `go build ./cmd/orch/...`, `make ci`. Verify `D`/`Shift+D` toggles (not while composing),
    mouse wheel/click, `y` copy, reflow of content+dock, persistence across tab switch, live
    streaming, git-reconciled final diff on completed runs, and OSC 52 over a remote connection.

---

## 11. Open questions → defaults (all revisitable)

- **Pane width** (`diffPaneWidth`): default **48 cells** (mirrors the GUI's 480px rail
  proportionally at a typical 96-col terminal). Revisitable; must not starve the main content.
- **Which screens can open the pane**: any screen the app has a diff-relevant owner for
  (Execution detail + Ask conversation detail per the GUI scope). Default → open on those two;
  on other screens the toggle is a no-op.
- **`⟪no newline⟫` marker**: reuse the GUI's exact glyph (`⟪no newline⟫`) so the logical output
  matches byte-for-byte.
- **Detail data source for the pane's owner**: the app already holds `execSessions` and
  `chatConvID`; the pane reads the owner id from the active screen's `DetailID()`/`m.chatConvID`
  exactly as the chat dock does.

---

## 12. FACTS LEARNED

- `FileEditService.GetSessionFileEdits`/`StreamFileEdits` **require** `request.tenant_id`
  non-empty (`internal/fileedit/rpc.go` → `CodeInvalidArgument` "tenant_id must not be empty"),
  UNLIKE the TUI's project/execution streams which pass `TenantId=""`. TUI must resolve its
  tenant via `Auth.ListIdentities` (returns `Identity.tenant_id`, resolves tenant from bearer)
  and cache it.
- The current worktree HEAD is a stale `develop` (`95b45fd27`) with no `internal/tui`, no
  lipgloss/bubbletea, and no diff pipeline. Base the implementation on `origin/develop`
  (`f3ccfd0fe`).
- The TUI shell's `Clients` has no `FileEdits` client yet; the generated one exists at
  `api/gen/go/orchicon/api/v1/apiv1connect/file_edit_service.connect.go`.
- The GUI's `parseUnifiedDiff`/`groupByFile`/`emphasizeTokens` (frontend/src/lib/diff/sideBySide.ts)
  are the byte-for-byte contract the Go `diffs` package must mirror; tests in both drive the same
  `internal/testfixtures/fileedit/*.json`.
- GUI DiffSidebar persists open/tab/selected via `usePersistentState` at the host; the TUI
  analogue is shell-owned state that survives `SwitchTo`.

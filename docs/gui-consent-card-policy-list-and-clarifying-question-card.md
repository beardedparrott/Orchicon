
# Implementation plan — GUI: consent card, policy list, and clarifying-question card

Work item: `GUI: consent card, policy list, and clarifying-question card`
Branch: `gui-consent-card-policy-list-and-clarifying-question-card-e3eyqqxb2m5t39y6`
Author step: Principal Software Architect (step 1 of 5). This file is the deliverable; the closing
numbered list is the implementation contract.

Every `path:line` anchor below was read in this session from **`origin/develop`** (`c912f640`).
The run branch was cut from a **stale local `develop`** — see step 1.

---

## 1. What already landed (do not rebuild it)

The sibling tasks this item depends on are **already merged into `origin/develop`**, not pending:

| Piece | Where | State |
|---|---|---|
| Consent core (pending asks, precedence, reply path, session grants) | `internal/askorchicon/consent.go`, `consent_service.go` (1077 + 86 lines) | landed (PR #586) |
| Persistent deny/accept policy (file) + its RPCs | `internal/permpolicy/policy.go`; `proto/orchicon/api/v1/settings_service.proto:48-96` (`GetPermissionPolicy` / `AddPermissionPolicyEntry` / `RemovePermissionPolicyEntry`) | landed |
| **GUI persistent deny/accept list (item 3)** | `frontend/src/components/PermissionsTab.tsx` (234 lines), `frontend/src/api/permissions.ts` (73 lines), mounted at `frontend/src/routes/settings.tsx:75`, tab declared at `settings.tsx:28,49` | **landed — no work needed** |
| Clarifying-question card (item 4, partial) | `frontend/src/components/ask/AskCard.tsx` (150 lines), rendered for a recorded `ask_user` call at `frontend/src/routes/ask-orchicon.tsx:1899-1914` | landed; **missing keyboard model + free-text "Other"** |
| Session-grant view model (TUI only) | `internal/tui/chat/consent.go:156-175` (`SessionGrant{Directory,Tool,Count}`, `PermissionStore.SessionGrants`) | landed; **no wire RPC exists** |

**Item 3 needs no GUI work.** `PermissionsTab` already mutates only through the plane and never keeps a
local copy (`permissions.ts:1-11`), names the policy file (`PermissionsTab.tsx:90-96`), splits
deny/accept (`permissions.ts:67-72`) and already states the precedence where it matters
(`PermissionsTab.tsx:158-162` plus the per-row line at `:175-179`: "a session grant cannot override this
entry"). Only verification (§5 step 22e).

## 2. The delta (four workstreams; three need new code)

**D1 — Consent card on the turn stream (the real gap).** The wire arm exists
(`proto/orchicon/api/v1/ask_orchicon_service.proto:292` — `PermissionAsk permission_ask = 9`) and the
server emits it (`internal/askorchicon/consent.go:1051-1070` `emitPermissionAsk`, called from
`chat.go:1876`), but the GUI **never reads it**: `runStream` (`ask-orchicon.tsx:864-924`) and `runWatch`
(`ask-orchicon.tsx:744-782`) switch on `turnStarted` / `textChunk` / `reasoning` / `heartbeat` / `error`
only. Nothing under `frontend/src` mentions `permissionAsk` or `replyPermissionAsk`.

**D2 — Session grants list + revoke (needs a small wire surface).** `grantStore` is in-memory and read
only by the guard shim (`internal/askorchicon/ask_guard.go:116` — `s.grants.Roots(convID)`); no RPC
lists or revokes a grant (the `AskOrchiconService` RPC list ends at `GetModelCapabilities`). The GUI
cannot invent this: revoke must change what the next tool call sees, which only the plane's store can do.
Also `grantStore` holds `map[dir]bool` (`consent.go:455`) — no timestamp, so "when granted" is not
representable today.

**D3 — One card, two variants, keyboard-first.** `AskCard` renders only the question shape; its options
are mouse buttons (`AskCard.tsx:120-136`) and the `allowOther` flag renders only a hint sentence
(`AskCard.tsx:139-143`) — no "Other" control, no arrow-key model, no Escape handling.

**D4 — Precedence visible on the card.** `decide()` refuses outright when a deny entry matches a target
(`consent.go:913-917` → `reject`, **no ask is emitted**), so a card is only raised for an action the deny
list does *not* refuse. The reachable precedence case is the *grant directory*: granting `/home/me` for
the session while the policy denies `~/.ssh/**` **below** it — the deny entry still wins
(`permpolicy/policy.go:349-356`: deny is evaluated before `SessionGranted` is even read). The card must
say that instead of implying the grant covers everything under the directory.

---

## 3. Files: new, changed, and the exact wiring point

### New files
| Path | Contents |
|---|---|
| `internal/askorchicon/consent_grants_service.go` | `ListPermissionGrants` + `RevokePermissionGrant` handlers, same shape as `consent_service.go:24-86` (`requireTenant`, empty-arg guards, `s.loadConversationRow` tenant check). |
| `internal/permpolicy/denybelow_test.go` | tests for the new `Store.DenyBelow`. |
| `frontend/src/lib/ask-consent.ts` | pure ask state machine: `AskItem`, `applyAskChunk`, `resolveAsk`, `outcomeLabel`, `pendingFor(asks)`. |
| `frontend/src/lib/ask-consent.test.ts` | unit tests for it (vitest, same style as `lib/ask-stream-group.test.ts`). |
| `frontend/src/components/ask/SessionGrants.tsx` | the compact grants disclosure (list + revoke). |
| `frontend/src/components/ask/SessionGrants.test.tsx` | render + revoke test (follow `AskCard.test.tsx`'s harness). |

### Changed files (anchor → change)
| Anchor | Change |
|---|---|
| `proto/orchicon/api/v1/ask_orchicon_service.proto:130` (after `ReplyPermissionAsk`) | add `rpc ListPermissionGrants(...)` and `rpc RevokePermissionGrant(...)`. |
| `proto/orchicon/api/v1/ask_orchicon_service.proto:422` (`string summary = 9;` ends `PermissionAsk` at 423) | add `repeated string deny_entries_below = 10;` — deny entries the grant directory does not override. |
| `proto/orchicon/api/v1/ask_orchicon_service.proto:~425` (before `enum PermissionChoice`) | add `SessionPermissionGrant{directory, granted_at_unix}`, `ListPermissionGrantsRequest/Response`, `RevokePermissionGrantRequest/Response`. |
| `internal/permpolicy/policy.go:349` (next to `Decide`/`Consult`) | add `func (s *Store) DenyBelow(dir string) ([]string, error)`: read the policy, expand each deny entry with the existing `expandHome` (`policy.go:254`), return the **literal** entries where `pat == dir` or `strings.HasPrefix(pat, dir+"/")`. Matching logic stays inside permpolicy (one copy of the rules). |
| `internal/askorchicon/consent.go:455` (`byConv map[string]map[string]bool`) | value type → `map[string]map[string]time.Time`; `Grant` (463) stores `time.Now()`; `Has` (478) becomes a presence check; `Roots` (511) keeps its contract (sorted keys) so `ask_guard.go:116` is untouched. |
| `internal/askorchicon/consent.go:511` (after `Roots`) | add `type sessionGrant struct{Directory string; GrantedAt time.Time}`, `func (g *grantStore) Revoke(convID, dir string) bool`, `func (g *grantStore) List(convID string) []sessionGrant` (sorted by directory). |
| `internal/askorchicon/consent.go:616-640` (`pendingAsk`) | add `DenyBelow []string`. |
| `internal/askorchicon/consent.go:940-969` (ask construction in `decide`) | set `ask.DenyBelow` from `denyBelowForGrant(pol, a.Key)` (a small helper wrapping `pol.DenyBelow`, error → log + empty, never fail the ask). |
| `internal/askorchicon/consent.go:1051-1070` (`emitPermissionAsk`) | carry `DenyEntriesBelow: a.DenyBelow` into the proto message. |
| `internal/askorchicon/chat.go:427` (right after `subID, ch := h.subscribe()`) | replay the conversation's open asks to the re-attached watcher: `for _, a := range s.pending.list(req.Msg.ConversationId) { emitPermissionAsk(...stream.Send...) }`. `WatchTurnStream` today only drains the hub, so a refresh/second tab loses a pending card. |
| `frontend/src/components/ask/AskCard.tsx` | add `AskCardShell` + `AskCardAction`; refactor `AskCard` onto it (keeping `ask-card` / `ask-card-error` / `data-answered` test ids and the existing props at `AskCard.tsx:24-40`); add the `Other…` inline free-text row; add `ConsentAskCard`. |
| `frontend/src/lib/ask-stream-group.ts` | no behavioural change needed — `groupStreamItems` already passes non-text items through (`ask-stream-group.ts:56-59`); the ask is **not** a stream item (see §4 step 12), so nothing is added here (see §5 step 13). |
| `frontend/src/routes/ask-orchicon.tsx:124-155` (`ConvStream` / `EMPTY_STREAM`) | add `asks: AskItem[]`. |
| `frontend/src/routes/ask-orchicon.tsx:333` (next to `setStream`) | add `applyAsk(convId, ask)` (dedupe by `askId`) and `handleAskDecision(convId, askId, choice)`. |
| `frontend/src/routes/ask-orchicon.tsx:864-924` (`runStream` switch) | add `else if (chunk.event.case === "permissionAsk") applyAsk(convId, chunk.event.value)`. |
| `frontend/src/routes/ask-orchicon.tsx:744-782` (`runWatch` switch) | same arm. |
| `frontend/src/routes/ask-orchicon.tsx:551-576` (re-attach effect) | after arming the slot, `void runWatch(activeConvId, pendingId, dispatchGenRef.current[activeConvId] ?? 0)` — today `runWatch` is reached only from `fail()` (`:854`), so a restored turn never re-dials and no ask can arrive. |
| `frontend/src/routes/ask-orchicon.tsx:1471-1472` (between the optimistic-user block and the live-stream block) | render `activeStream?.asks` as `ConsentAskCard`s, **outside** the `isStreaming &&` guard so a resolved card stays in the transcript after the turn. |
| `frontend/src/routes/ask-orchicon.tsx:1349` (the desktop header row: `flex items-center justify-between border-b …`) | mount `<SessionGrants conversationId={activeConvId} />` in that row, next to the diff-sidebar toggle (`:1362`). |
| `frontend/src/routes/ask-orchicon.tsx:311-328` (existing document keydown effect) | add Escape-means-Deny while a pending ask exists (§4 step 15). |
| `frontend/src/api/askOrchicon.ts:8-13` (`askKeys`) | add `grants: (id) => ["ask","grants",id]`; add `useListPermissionGrants` / `useRevokePermissionGrant` (invalidate + `setQueryData` from the response, mirroring `permissions.ts:36-60`). |
| `internal/askorchicon/consent_test.go`, `consent_wire_test.go` | store revoke/list/timestamp, `deny_entries_below` on the emitted ask, grants RPC behaviours. |

## 4. Keys, accessibility and honest limits (decisions)

1. **DECISION:** the ask is **not** a `StreamItem`; it lives in `ConvStream.asks`, which no
   turn-lifecycle updater clears (`ask-orchicon.tsx:521-530`, `:701-710`, `:941-950`, `:965-975`).
   Rationale: `items` is emptied when a turn settles, which would erase the outcome the acceptance
   criteria require in the transcript.
2. **DECISION:** the ask card is a **pending transcript row, never a modal** — the composer, the
   sidebar and the Stop button stay live while an ask is outstanding (the turn is opencode's, not ours;
   `chat.go:1841-1845`).
3. **DECISION:** `Escape` = Deny, never a dismissal — the card's own `onKeyDown` handles it when focus
   is inside the card; a document-level listener (guarded by "a pending ask exists for the active
   conversation") handles it otherwise, and skips events whose `defaultPrevented` is already true so one
   Escape never denies twice. On the **question** card Escape only collapses the inline "Other" input and
   does nothing otherwise (it must not silently settle a question).
4. **DECISION:** arrow keys move a roving focus across `[data-ask-action]` buttons; Enter/Space activate
   (native button behaviour). No new key chord, no global registry.
5. **DECISION:** the wire gains exactly two RPCs and one field. Grants stay **in-memory and
   conversation-scoped** (the landed design, `consent.go:450-452`); revoke therefore takes effect on the
   next tool call through the guard shim's existing read (`ask_guard.go:116`) with no restart.
6. **DECISION (revisitable):** `deny_entries_below` is a *literal-prefix* test on the policy entries, not
   a filesystem walk. It answers "does a deny entry live under the directory I am about to grant?" —
   which is the only question a card can answer without enumerating the disk.
7. **DECISION:** `ConsentAskCard` never renders "Allow for this session" as *disabled*; when
   `deny_entries_below` is non-empty it renders the action plus an explicit line naming the entries
   ("your policy still refuses `~/.ssh/**` here — a session grant never overrides it"). Suppressing the
   action outright would be wrong: the grant does cover the rest of the directory.
8. **DECISION:** the persistent deny/accept list gets **no GUI work** (landed, see §1); the plan's only
   obligation there is verification that a GUI-added entry reaches the TUI/file.
9. **LIMIT (documented in the code comment):** `ConvStream.asks` is page-lifetime state. After a reload
   the card is recovered only because step 8 replays open asks on re-attach **and** step 15 re-dials the
   watch; a reload while the plane holds no live turn shows no card (the turn is gone, and its held ask
   was `reject`ed by `finalize`, `consent.go:1000-1005`).

---

## 5. Numbered implementation list (mechanically executable — no re-planning needed)

1. **Sync the branch first.** `git fetch origin && git merge origin/develop` (HEAD `9488ca29` is 41
   commits behind `origin/develop` `c912f640`, which already contains the consent core, `AskCard.tsx`,
   `PermissionsTab.tsx`, `permissions.ts`). Re-run `git log --oneline -3` and confirm `c912f640`'s
   content is present. **Do not rebuild any landed piece.**
2. `proto/orchicon/api/v1/ask_orchicon_service.proto`: add the two RPCs after `ReplyPermissionAsk`
   (:130); add `repeated string deny_entries_below = 10;` to `PermissionAsk` (:422); add
   `SessionPermissionGrant` + the four request/response messages before `enum PermissionChoice`.
3. `make gen` (needs `make tools` for the pinned buf; the BSR remote plugins need network). Confirm the
   diff touches only `api/gen/go/orchicon/api/v1/*` and `frontend/src/api/gen/orchicon/api/v1/*`. If buf
   cannot reach the BSR, **stop and report** — do not hand-edit generated files.
4. `internal/permpolicy/policy.go`: add `Store.DenyBelow(dir string) ([]string, error)` reusing
   `expandHome` (:254); unit-test it in `internal/permpolicy/denybelow_test.go` (entry equal to the dir,
   entry below the dir, `~/.ssh/**`, an unrelated entry, a malformed file → error not silent success).
5. `internal/askorchicon/consent.go`: migrate `grantStore` to `map[string]map[string]time.Time`; keep
   `Grant`/`Has`/`Roots`/`Len`/`ClearConversation` signatures; add `Revoke` + `List`.
6. `internal/askorchicon/consent.go`: add `pendingAsk.DenyBelow`; in `decide()` (near :946) populate it
   via `denyBelowForGrant(pol, a.Key)`; carry it in `emitPermissionAsk` (:1051-1070).
7. New `internal/askorchicon/consent_grants_service.go`: `ListPermissionGrants` (validate
   `conversation_id`, tenant-check via `loadConversationRow` like `consent_service.go:45-49`, return
   `SessionPermissionGrant`s from `s.grants.List`) and `RevokePermissionGrant` (validate both fields,
   `removed := s.grants.Revoke(...)`, always return the refreshed list; `removed == false` for an
   unknown directory, never a silent success).
8. `internal/askorchicon/chat.go`: in `WatchTurnStream`, right after `h.subscribe()` (:427), emit each
   `s.pending.list(conversationID)` ask to the new stream via `emitPermissionAsk`, logging a warning if
   `stream.Send` fails. Guard with a comment that this is the re-attach recovery path.
9. Go tests: extend `consent_test.go` (revoke removes the grant a later `Has`/`Roots` no longer sees;
   `List` carries the grant time, zero when unknown) and `consent_wire_test.go` (accepting with a deny
   entry below the key lands `deny_entries_below` — extend the existing `emitPermissionAsk` assertions at
   `consent_wire_test.go:42-99`); add a `RevokePermissionGrant` service test (applied → list shrinks;
   unknown dir → `removed=false`; the turn's next `decide` asks again because `Has` is false). Run
   `go test ./internal/permpolicy/... ./internal/askorchicon/...`.
10. `frontend/src/components/ask/AskCard.tsx`: extract `AskCardShell` (container + uppercase header +
    body + action column, styling lifted from `AskCard.tsx:103-137`) and `AskCardAction`; re-implement
    `AskCard` on it with its current props/test-ids so `AskCard.test.tsx` keeps passing.
11. `AskCard.tsx`: add the `Other…` action row (rendered only when `allowOther`) that expands an inline
    `<input data-testid="ask-card-other">`; Enter submits `onSelect(value)` (the caller sends it as a
    normal user message, `ask-orchicon.tsx:1455`), Escape collapses it; keep the existing hint sentence
    for the `answered` state. Add `ConsentAskCard` with props `{ask, resolved?, busy?, onDecide(choice),
    onEscape}` rendering tool + target/command + directory + the inside-project line + the
    `deny_entries_below` note, and settling into the outcome label when `resolved` is set.
12. `AskCardShell`: implement the roving-focus keyboard model (ArrowUp/ArrowDown over
    `[data-ask-action]`, Enter/Space activate) and `Escape` → `props.onEscape` (never self-dismiss).
13. New `frontend/src/lib/ask-consent.ts`: `AskItem {key, ask, outcome}`, `applyAskChunk` (dedupe by
    `askId` — the live socket and the watch socket can both deliver it), `resolveAsk(items, askId,
    outcome, at)`, `outcomeLabel(tool, summary, outcome)` producing the transcript line ("Allowed once —
    write /path", "Denied — bash: …"), `pendingFor(items)`. Unit-test in `ask-consent.test.ts`.
14. `frontend/src/routes/ask-orchicon.tsx`: add `asks: AskItem[]` to `ConvStream` (:124-145) and
    `EMPTY_STREAM` (:146-155); add `applyAsk` + `handleAskDecision` next to `setStream` (:333).
    `handleAskDecision` calls `askOrchiconClient.replyPermissionAsk({conversationId, askId, choice})`,
    resolves the item in place on `applied`, resolves as `expired` (with the server's `detail`) when
    `expired` is true, toasts and leaves it pending on a thrown error, and invalidates `askKeys.grants`
    after an `ALLOW_SESSION` decision.
15. `ask-orchicon.tsx`: add the `permissionAsk` arm to BOTH switches (`runStream` :864-924 and
    `runWatch` :744-782) calling `applyAsk`; in the re-attach effect (:551-576) call
    `runWatch(activeConvId, pendingId, gen)` after arming the slot.
16. `ask-orchicon.tsx`: render the cards at :1471-1472 (between the optimistic-user block and the live
    stream block) outside the `isStreaming` guard, mapping `activeStream?.asks` to `ConsentAskCard`, with
    `onDecide={(c) => void handleAskDecision(activeConvId, item.ask.askId, c)}`.
17. `ask-orchicon.tsx`: add the Escape-means-Deny document listener next to the existing one (:311-328),
    active only while `pendingFor(activeStream?.asks).length > 0`, denying the oldest pending ask and
    returning early when `e.defaultPrevented`.
18. `frontend/src/api/askOrchicon.ts`: add `askKeys.grants(id)` and
    `useListPermissionGrants(convId)` / `useRevokePermissionGrant(convId)`.
19. New `frontend/src/components/ask/SessionGrants.tsx`: a small disclosure button ("Grants (n)")
    rendering the directory, a relative "granted <n>m ago" from `granted_at_unix`, and a Revoke button
    per row; empty state "No session grants for this conversation"; Escape closes the panel; mount it in
    the header row at `ask-orchicon.tsx:1349`.
20. Frontend tests: extend `AskCard.test.tsx` (Other input submits; ArrowDown+Enter selects; Escape calls
    `onEscape`; `ConsentAskCard` renders the three actions, the deny note, and the settled state) and add
    `SessionGrants.test.tsx` (list renders, revoke fires the mutation, empty state). Harness: the suite
    ships **only `vitest`** (no `@testing-library/*` in `frontend/package.json`) — copy the rendering
    approach already used by `AskCard.test.tsx` rather than adding a dependency.
21. Verification (all commands from `frontend/` unless noted): `npx vitest run src/lib/ask-consent.test.ts
    src/components/ask/AskCard.test.tsx src/components/ask/SessionGrants.test.tsx`; `npx tsc -b`;
    `npx eslint src/components/ask src/lib/ask-consent.ts src/routes/ask-orchicon.tsx`; from the repo
    root `go build ./... && go test ./internal/permpolicy/... ./internal/askorchicon/...`.
22. Acceptance walk-through on the container sandbox plane (never prod): (a) a write outside the project
    raises a card naming `write` and the path with exactly three actions; (b) Allow once resolves in
    place and the turn proceeds; (c) Allow for this session on a directory silences further cards for
    that directory, and a card still appears for a different one; (d) the grants strip lists the
    directory and Revoke makes the next write ask again; (e) add a deny entry in Settings → Permissions,
    confirm the file on disk changed and `permpolicy.DefaultPath()` is what the TUI reads; press Escape
    on a fresh card and confirm a Deny outcome lands in the transcript; (f) an `ask_user` turn renders
    the question card, options select on Enter, and the "Other" free text is sent as the next user
    message.

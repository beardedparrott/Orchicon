# TUI: consent card, policy list, and clarifying-question card parity — implementation plan

Work item: `tui-consent-card-policy-list-and-clarifying-question-card-parity-fgacc79vsnmj0fvx`
Branch: `tui-consent-card-policy-list-and-clarifying-question-card-parity-fgacc79vsnmj0fvx` (off `develop`)
Author: Principal Software Architect (step 1 of 5). This plan is the deliverable; implementation is steps 2–5.

> **Plan path note.** The repo's plan/notes directory is `docs/` (see `docs/tui-parity.md`,
> `docs/tui-diff-sidebar-slide-out-pane-consuming-the-shared-pipeline.md` — a prior plan doc). `architecture-notes/`
> does not exist in this branch, so this plan lives beside the other TUI notes in `docs/`.

> **Scope split.** Storage of the persistent allow/deny list, the permission decision itself, and the ask emission
> on the wire are owned by the SIBLING tasks (`internal/askorchicon`, `internal/askmode`, `proto/`). This plan owns
> **the TUI half only**: the card, its key ownership, the grant roll-up, the list/CRUD surfaces, the
> clarifying-question card, and the TUI-side tests. Where this plan names a wire surface it is a *contract the TUI
> consumes*, not a change this plan makes.

---

## 1. Ground truth read this session (file:line proof)

Shell key routing (the vocabulary the task tells us to reuse):

- `internal/tui/router.go:729-733` — the `ClaimsKeys` gate: `if ks, ok := m.screens[m.active].(interface{ ClaimsKeys() bool }); ok && ks.ClaimsKeys() { return m.passToScreen(msg) }`. A screen that claims gets EVERY key before any shell chord (`q`/space/`/`/`d` are all intercepted later).
- `internal/tui/router.go:595-598` — the `FormOpen` gate (Tab moves form fields, not tabs).
- `internal/tui/router.go:620-628` — the `ctrl+g` focus chord calls `DropKeyClaim()` on the screen.
- `internal/tui/keyclaim_test.go:31-60` — `claimStub` + `TestScreenKeyClaimBypassesShellRoutes`: the existing assertion shape for "a claimed key must not quit / must not open the tab menu / must not open the palette". Extended, not replaced.
- `internal/tui/screens/kit2/base.go:755-767` — `DropKeyClaim()`; the doc comment states a claim is a **latch** and that the focus chord must be able to release it.
- `internal/tui/screens/kit2/base.go:935-1010` — `Base.key()` order: `b.Open` dialog (owns EVERY key) → `editForm` → `filtering`. Nothing else on the screen sees a key while one is up.
- `internal/tui/screens/enforcement/overlay.go:1-8` — the recorded precedent for a screen-owned modal: "Every overlay CLAIMS the keyboard (Model.ClaimsKeys) while it is open", with `ovKind` state + `screen.go` painting it and `screen.go:150-154` `ClaimsKeys() bool { return m.ov != nil }`.
- `UPDATES.md:53` (row 274) — the recorded rule: "a mode the operator cannot tell they are in is worse than no mode"; `internal/tui/screens/execution/actions.go:574` repeats it. The card must be visible while it owns keys.

kit2 primitives (render through these, not bespoke strings):

- `internal/tui/screens/kit2/dialog.go:14-91` — `Dialog{Title,Body,Buttons,Sel,Danger}`, `NewDialog`, `HandleKey` (esc → dismissed, left/right → move, enter → choice), `Box(w,h)` (rounded border, `theme.PanelBgStyle`, `Pad`). **Horizontal** button row.
- `internal/tui/screens/kit2/dialog.go:169-185` — `Splice` / `Center(base, box, width, height)` overlay helpers.
- `internal/tui/screens/kit2/panel.go:23-60` + `:212 Pad` — `Panel` (exactly W×H cells) and the shared padding helper.
- `internal/tui/screens/kit2/form.go:228 NewForm` + `FieldSpec`/`KText`/`KTextArea` (single-line input widget for the "Other" free text).
- `internal/tui/screens/kit2/table.go:160 NewTable` (list surface for the grant roll-up and the allow/deny list).
- `internal/tui/screens/kit2/keys.go:7` — `type keyMsg = tea.KeyMsg` (kit2 widgets take keys without importing bubbletea).
- `internal/tui/screens/kit2/base.go:1357-1377` — `BulkThreshold`, `MarkedIDs`, `MarkCount` (selection vocabulary, for the list surfaces).

Transcript (where the card must appear):

- `internal/tui/chat/grouping.go:15-27` — `ItemKind` + `KindUser/KindText/KindTool/KindReasoning/KindError/KindArtifact/KindSession`.
- `internal/tui/chat/grouping.go:38-70` — `ChatItem` (one struct, `Kind` selects the shape).
- `internal/tui/chat/view.go:105-115` — `copyTextFor` switches on `it.Kind`; `:166-220` — `RenderItems`'s per-kind render `switch it.Kind` (`case KindTool:` at `:207`). This is the insertion point for the card's transcript rendering.
- `internal/tui/chat/controller.go:1035-1095` — the stream event → `ChatItem` mapping (`case *apiv1.ChatStreamResponse_ToolCallStart:` at `:1075`). This is where the ask event becomes a pending card item.
- `internal/tui/chat/controller.go:677-700` — the persisted-history → `ChatItem` mapping (so a RESOLVED card row survives a reload instead of vanishing).
- `internal/tui/app.go:3124-3160` — `onChatWake`: the shell repaints the transcript via `askS.RenderTranscript(items, live, haveLive)` then `SetDetailContentLaidOut`. The screen already receives the full item list on every wake — that is the reliable reconciliation channel for "is the ask still pending?".
- `internal/tui/app.go:3788` — `RepaintTranscript()` (the screen-triggered repaint the ask screen already calls).
- `internal/tui/app.go:4059-4060` — the composer's single send funnel (`sendChat(...)` + `onChatWake()`); the clarifying-question card must reuse it.
- `internal/tui/app.go:1608` — `OpenAskConversation(id)` — the existing "screen asks the shell to act" interface shape.
- `internal/tui/screens/ask/screen.go:36-70` (`Model`, `New`), `:126-146` (`detail()`), `:150-165` (`Update`), `:167-181` (`View`), `:253-300` (`RenderTranscript`) — the screen that owns the card. It does **not** implement `ClaimsKeys`/`DropKeyClaim`/`FormOpen` today (verified: no hits in that file).

Slash registry + chord discipline:

- `internal/tui/slash.go:23-40` (`SlashCommand{Name,Usage,Desc,Aliases,MinArgs,Run}`), `:99-120` (`buildSlashRegistry`), `:461 resolve`, `:504` run site.
- `internal/tui/app.go:548` — `m.slash = buildSlashRegistry(m)` (wiring point for new commands).
- `UPDATES.md:31/24/27` (rows 296/300/303) — the recorded rule that the conversations rail's keys are **composer-driven** and therefore all of its chords are modifier-based; a bare letter there is *typed*. Any new rail/pane-affecting key must be a modifier chord or a slash command. **Decision: the new surfaces are slash commands + modal keys, so no bare-letter chord is added to the rail.**

Wire surface (owned by siblings; consumed here):

- `proto/orchicon/api/v1/ask_orchicon_service.proto:270-279` — `ChatStreamResponse.oneof event { text_chunk | tool_call_start | tool_call_result | error | done | turn_started | reasoning | heartbeat }`. The ask must ride this oneof.
- `proto/orchicon/api/v1/ask_orchicon.proto:183-240` — the chunk message family (`TextChunk`, `ToolCallChunk`, `ToolCallResult`, `ErrorChunk`, `DoneSignal`, `ReasoningChunk`, `Heartbeat`).
- `internal/askorchicon/ask_guard.go:1-70` — the Ask path's existing OS backstop (`guard.NewExecutionGuard("")`, shared host-serve mode). The permission decision sits *above* it on the sibling's side.
- `internal/askmode/askmode.go:60-120` — the "one table, no adapter owns it" pattern for a tool policy; the sibling policy store follows the same shape.

---

## 2. Decisions (one line each; `DECISION (revisitable)` where the sibling task could force a change)

1. **Card carrier = a new stream event.** The ask arrives as a new arm on the `ChatStreamResponse` oneof (`proto/orchicon/api/v1/ask_orchicon_service.proto:270-279`) and becomes a `ChatItem` in `internal/tui/chat/controller.go`'s event switch. `DECISION (revisitable)`: if the sibling lands a different event name/shape, only `controller.go`'s new `case` adapts — the TUI models the ask as `chat.PermissionAsk` (its own struct), never as the proto type.
2. **Card as a transcript item, not a floating dialog.** A pending ask is `ItemKind` `"consent"` carrying tool/target/directory/options/cursor/decision, rendered inside the transcript body. Reason: acceptance says the card *renders in the transcript*, and the transcript is the one renderer the shell repaints on every wake (`app.go:3124`).
3. **New kit2 widget `kit2.Card`**, in-package with the primitives, used by the transcript renderer. Reason: `Dialog` (`dialog.go:14-91`) is a single horizontal button line driven by left/right; the three action labels do not fit a narrow pane and the requirement names arrow keys + Enter. `DECISION (revisitable)`: vertical option list; `Dialog` remains for confirms.
4. **Keys are owned by the screen, state by the item.** `ask.Model.ClaimsKeys()` returns true while a card is pending (the `router.go:729-733` gate), and the screen's `Update` moves the cursor on the item and handles Enter/Esc. Reason: one owner for keys + one renderer keeps selected-row and action from drifting.
5. **`DropKeyClaim` while a card is pending = DENY.** Reason: the claim is a latch (`kit2/base.go:755-767`) and `ctrl+g` is the chord that must always be able to leave it; denying is already the Esc outcome, so `ctrl+g` cannot silently dismiss a decision. `DECISION (revisitable)`: a no-op instead would let `ctrl+g` put focus in a composer the card still claims — worse.
6. **Escape = Deny** (single arm, no separate "dismiss"), and the resolution is appended to the transcript as a resolved `consent` item. Reason: acceptance ("Escape denies … the refusal is recorded in the transcript").
7. **The card is reconciled from the item list, not a second latch.** `ask.Model.RenderTranscript` (called by the shell on every wake, `app.go:3124-3160`) compares the pending ids it is handed with its own state: an ask the transcript no longer carries (turn ended, superseded, aborted) drops the card. Reason: "releasing them must be reliable" and "the card must not wedge the turn".
8. **Wording is three exported TUI constants, asserted as LITERALS.** `ConsentAllowOnce = "Allow once"`, `ConsentAllowSession = "Allow for this session"`, `ConsentDeny = "Deny"` (`UPDATES.md:24` row 303's recorded rule: compare-to-itself passes at any value). A cross-client test reads the GUI's source and asserts the same three strings (precedent for reading `frontend/src` from a Go test: `internal/fileedit/diff_test.go`, `internal/tui/chat/grouping_test.go`).
9. **Session grants are plane state, displayed and revoked by the TUI.** A `grants: N dir(s) — /grants` field row on the conversation detail + `/grants` opens a `kit2.Table` overlay with Enter = revoke (behind `kit2.Confirm`). Reason: "ask once per directory per session" is enforced where the bash tool runs (the plane), so the TUI must not hold the only copy. `DECISION (revisitable)`: the read/revoke RPC names are the sibling's; if absent, the TUI renders the field as `grants: (unavailable on this plane)` rather than a fabricated count.
10. **Persistent list is `/permissions`, NOT `/policy`.** The GUI's `/policies` route is the Rego policy engine (`frontend/src/routes/policies.tsx`) — a different concept; reusing the word would be a parity lie.
11. **Denied-by-file is stated, never offered.** A card whose target matches a persistent DENY entry renders a notice line and a DISABLED "Allow for this session" row naming the pattern. Reason: acceptance ("say that a session grant cannot override it rather than offering a grant that will be refused").
12. **Clarifying question = the same primitive, different content**, and selecting sends through the existing composer funnel (`app.go:4059-4060`) via a new `SendUserMessage(text) tea.Cmd` interface on the App (shape of `OpenAskConversation`, `app.go:1608`). Reason: one send path; the acceptance says the choice is sent as "the next user message".
13. **"Other" is an extra row that reveals a one-line input inside the card** (`kit2.Form`-styled, one `KText` field). Esc in the input returns to the option list (state stays visible); Esc on the option list denies. `DECISION (revisitable)`: Esc from the input could deny directly, but then a typo would deny the tool for the session.
14. **No new bare-letter chord anywhere.** New surfaces are slash commands or modifier chords; asserts in `internal/tui/composer_hint_test.go`-style hints.

---

## 3. New files (exact paths + contents)

1. **`internal/tui/chat/consent.go`** — the TUI's own ask model + the wording constants.
   - `const (ConsentAllowOnce = "Allow once"; ConsentAllowSession = "Allow for this session"; ConsentDeny = "Deny")`.
   - `type PermissionAsk struct { ID, Tool, Target, Directory string; Kind AskKind; Question string; Options []string; AllowOther bool; DeniedBy string /* the file pattern that denies this target, "" when none */ }` with `type AskKind string` (`AskTool`/`AskQuestion`).
   - `func (a PermissionAsk) OptionLabels() []string` — the tool ask returns the three constants (with the session row disabled when `DeniedBy != ""`); the question ask returns `Options` + `"Other"` when `AllowOther`.
   - `type ConsentDecision string` (`DecisionAllowOnce` / `DecisionAllowSession` / `DecisionDeny`).
   - `func ConsentItem(a PermissionAsk) ChatItem` — builds `ChatItem{Kind: KindConsent, AskID: a.ID, Sel: 0, Consent: &ConsentState{...}}`.
2. **`internal/tui/chat/consent_render.go`** — the transcript renderer for the item (called from `view.go`).
   - `func consentLines(it ChatItem, width int) []string` → delegates the *box* to kit2 (`kit2.CardLines`, below) so padding/border/selection come from kit2; a resolved item (decision set) renders one compact record row instead (`consent denied · write /etc/hosts`).
3. **`internal/tui/screens/kit2/card.go`** — the shared card widget.
   - `type CardLine struct { Text string; Detail string; Selected, Disabled bool }`.
   - `type CardSpec struct { Title, Body, Notice, Input string; Lines []CardLine }`.
   - `func CardLines(spec CardSpec, width int) []string` — rounded-border box (`dialog.go:60-84`'s styling: `theme.AccentIndigo`/`theme.Err` border, `theme.PanelBgStyle`, `Pad`) with one row per `CardLine`, the selected row in `theme.ListItemSelected`, a disabled row in `theme.HintText` with its `Detail` reason appended, and — when `Input != ""` or the input mode is on — a `"> "`+text row.
   - `func CardWidth(avail, want int) int` — clamps to the pane (mirrors `Dialog.Box`'s min-size enforcement).
   - `func (c *Card) HandleKey(k keyMsg) (choice int, closed bool)` on `type Card struct { Sel int; Lines []CardLine; Input bool }` — `up`/`down` (and `k`/`j`) move skipping `Disabled` rows, `enter` returns the selected index, `esc` returns `-1, true`.
4. **`internal/tui/screens/ask/consent.go`** — the Ask screen's card state machine + key routing.
   - `func (m *Model) pendingConsent() (ChatItem, bool)`; `func (m *Model) consentSel(delta int)`; `func (m *Model) resolveConsent(decision chat.ConsentDecision) tea.Cmd` (marks the item resolved, calls the shell's send/decide interface, appends the record row, clears the claim, sets a dock notice).
   - `func (m *Model) SyncTranscriptConsent(items []chat.ChatItem)` — reconciliation from decision 7.
   - `func (m *Model) ClaimsKeys() bool { return m.consentPending || m.ov != nil }`, `func (m *Model) DropKeyClaim()` (decision 5), `func (m *Model) FormOpen() bool` (false while a card is up — a card is not a form; the Tab chord must not claim otherwise).
5. **`internal/tui/screens/ask/cards.go`** — the question card + "Other" input handling (shares `kit2.Card`), and `func (m *Model) submitAnswer(text string) tea.Cmd` calling the shell's `SendUserMessage`.
6. **`internal/tui/screens/ask/policy.go`** — the grant roll-up + the persistent allow/deny list surfaces: `grantListTable(grants []Grant) *kit2.Table`, `policyListTable(entries []Rule) *kit2.Table`, `policyAddForm() *kit2.Form`, and the odd `askOverlay` state (`ovGrants`, `ovPolicyList`, `ovPolicyAdd`) mirroring `internal/tui/screens/enforcement/overlay.go`'s `ovKind` shape.
7. **Tests** (new): `internal/tui/consent_card_test.go` (shell-level key ownership, parity wording), `internal/tui/screens/kit2/card_test.go`, `internal/tui/chat/consent_test.go` (item → lines, resolved record row), `internal/tui/screens/ask/consent_test.go` (cursor/Enter/Esc/deny recording/reconcile-on-turn-end/Other input), `internal/tui/screens/ask/policy_test.go` (grant roll-up field, revoke, list CRUD, denied-target card).

---

## 4. Wiring (file + insertion point)

| Wire | File:anchor | What goes in |
|---|---|---|
| Stream event → item | `internal/tui/chat/controller.go:1090` (after `case *apiv1.ChatStreamResponse_ToolCallResult:`) | new `case *apiv1.ChatStreamResponse_<AskEvent>:` → `c.appendChunk(convID, chat.ConsentItem(...))`; a "resolved"/"cancelled" arm sets the decision on the existing item by `AskID` |
| History replay → item | `internal/tui/chat/controller.go:677-700` | map the persisted ask/decision metadata into a resolved `KindConsent` item so the record survives a reload |
| Item field | `internal/tui/chat/grouping.go:38-70` | add `AskID string`, `Sel int`, `Consent *ConsentState` to `ChatItem` |
| Item kind | `internal/tui/chat/grouping.go:15-27` | add `KindConsent ItemKind = "consent"` |
| Render dispatch | `internal/tui/chat/view.go:166-220` (`switch it.Kind`), next to `case KindTool:` at `:207` | `case KindConsent: lines = consentLines(it, width)` |
| Copy text | `internal/tui/chat/view.go:105-115` `copyTextFor` | a resolved consent item copies its one-line decision |
| Screen key path | `internal/tui/screens/ask/screen.go:150-165` (`Update`) | before `m.Base.Update(msg)`: if `m.consentPending`, route the key to `ask/consent.go`'s handler and return |
| Screen hooks | `internal/tui/screens/ask/screen.go` (end of file, with the other interfaces) | `ClaimsKeys()`, `DropKeyClaim()`, `FormOpen()` (new methods on `*Model`) |
| Reconcile | `internal/tui/screens/ask/screen.go:253-300` (`RenderTranscript`) | first line: `m.SyncTranscriptConsent(items)` |
| Grant field row | `internal/tui/screens/ask/screen.go:126-146` (`detail()`, the `fields` slice) | `{Key: "grants", Value: ...}` + `setFieldValue` in `RenderTranscript` |
| Slash commands | `internal/tui/slash.go:99-120` (`buildSlashRegistry`) | `add(SlashCommand{Name: "/grants", …})` and `add(SlashCommand{Name: "/permissions", Aliases: []string{"/perm"}, …})`, each running `m.RunAskOverlay(...)`; registered automatically through `app.go:548` |
| Send funnel for the question card | `internal/tui/app.go:4059-4060` | export `func (m *App) SendUserMessage(text string) tea.Cmd { return tea.Batch(m.sendChat(m.chatConvID, text, m.composerPreamble()), m.onChatWake()) }` (preamble source: the existing composer send path at `:4059`) and add the interface assertion in `ask/cards.go` |
| Hint | `internal/tui/screens/ask/screen.go:167-181` (`View`) | add `"/grants · /permissions: session grants and the persistent allow/deny list"` to the hint lines (composer-driven chord rule: slash, not a bare letter) |

---

## 5. Numbered implementation list (mechanically executable)

1. **Add the item + wording.** `internal/tui/chat/consent.go`: `PermissionAsk`, `AskKind`, `ConsentDecision`, `ConsentState`, the three wording constants, `OptionLabels()`, `ConsentItem()`. Add `KindConsent` to `grouping.go:15-27` and `AskID`/`Sel`/`Consent`/`Question`/`Options`/`Decision` to `ChatItem` (`grouping.go:38`).
2. **Build the kit2 card.** New `internal/tui/screens/kit2/card.go` (`CardSpec`, `CardLine`, `CardLines`, `CardWidth`, `Card.HandleKey`) reusing `dialog.go`'s border/pad/selection styles; `card_test.go` asserts: exactly `width` cells per line, the selected row is the only one in `theme.ListItemSelected`, a disabled row carries its reason, up/down skip disabled rows, `esc` closes, `enter` returns the index.
3. **Render the card in the transcript.** New `internal/tui/chat/consent_render.go`; wire `case KindConsent:` into `view.go:166-220` and the copy text in `copyTextFor`. Pending → the card box (tool + target + three rows); resolved → one record row (`allow once · write /home/.../x.go`, `deny · bash "make ci"`, `allow for this session · /dir (session)`).
4. **Carry the ask on the stream.** `internal/tui/chat/controller.go`: a new `case` in the `ChatStreamResponse` switch (after `:1090`) mapping the sibling's ask event → `ConsentItem`, and a resolve/cancel arm updating the item by `AskID`; plus the history path at `:677-700`. If the proto arm is not yet present, gate the `case` behind the generated type once the sibling lands it (do not invent a proto change here).
5. **Own the keys.** `internal/tui/screens/ask/consent.go` + three new methods on `ask.Model` (`ClaimsKeys`, `DropKeyClaim`, `FormOpen`); route keys in `ask/screen.go:Update` ahead of `Base.Update`; `SyncTranscriptConsent` at the top of `RenderTranscript` (`screen.go:253`).
6. **Wire the shell assertions.** `internal/tui/consent_card_test.go`, extending `keyclaim_test.go`'s `claimStub` shape with a consent stub: with a card pending, `q` does not quit, space does not open the tab menu, `/` does not open the palette, and the rune does not reach the composer; after resolve the same keys behave normally again; `ctrl+g` while pending records a DENY (decision 5).
7. **Resolve semantics.** Enter on a row: `Allow once` → `DecisionAllowOnce`; `Allow for this session` → `DecisionAllowSession` (scoped to the card's `Directory`); `Deny` / `esc` → `DecisionDeny`; each sends the decision through the shell, marks the item resolved so the transcript shows it, clears `consentPending` (release the claim), and sets a dock notice. A disabled session row cannot be selected.
8. **Turn-end release.** Assert `SyncTranscriptConsent` drops the card when the item list no longer carries the pending `AskID` (turn done/aborted/superseded), that the composer becomes typable again, and that `ClaimsKeys()` is false.
9. **Denied-by-file.** `PermissionAsk.DeniedBy` (set from the fetched rule list at card-build time): the card renders the notice line and disables the session row with the pattern named; a test asserts the disabled row is unreachable by arrows AND that no session-grant request is ever sent for that card.
10. **Session-grant roll-up.** Ask the plane for the conversation's grants (sibling RPC; else render `unavailable on this plane`): a `grants` field row in `detail()` (`screen.go:126-146`) and `/grants` opening `askOverlay{ovGrants}` — a `kit2.Table` of `directory · tool scope · count`, Enter → `kit2.Confirm` → revoke → refresh, with the table empty-state naming that no grant is active.
11. **Persistent list.** `/permissions` → `askOverlay{ovPolicyList}` (`kit2.Table`: effect, tool, pattern, source) with `a` opening `askOverlay{ovPolicyAdd}` (`kit2.Form`: effect picker, tool, pattern) that writes through the sibling's upsert and re-reads the list; Enter on a row → `kit2.Confirm` → delete → re-read. The overlay footer states "the file is the source of truth — GUI and hand-edits see the same list".
12. **Clarifying-question card.** Same item kind, `AskKind AskQuestion`: question body + options + `Other`. Enter on an option → `m.submitAnswer(text)` → shell's `SendUserMessage` (`app.go:4059-4060`), mark resolved. Enter on `Other` → input mode (a `> ` row inside the card); runes/backspace edit; Enter sends the typed text; Esc returns to the options (decision 13).
13. **Wording parity test.** Assert the three literals in `internal/tui/consent_card_test.go` (not constant-to-constant), and add a cross-client assertion that reads the GUI's source (`frontend/src/routes/ask-orchicon.tsx` or wherever the sibling lands the labels) and requires the same three strings — the repo has precedent for reading `frontend/src` from Go tests (`internal/fileedit/diff_test.go`, `internal/tui/chat/grouping_test.go`). If the GUI's labels land later, keep the test name and mark it with the sibling work item so it cannot be silently dropped.
14. **Hints + docs.** Add the `/grants · /permissions` hint line (`ask/screen.go:View`), the card's own footer (`↑/↓ select · enter confirm · esc denies`), and a `UPDATES.md` row in the repo's register once the feature lands (steps 3–5).
15. **Gate.** `gofmt -l`, `go build ./...`, `go vet ./internal/tui/...`, `go test ./internal/tui/... ./internal/tui/chat/...` green; every new behaviour proven by disabling it first (the row-303/296 convention), and the TUI-only claim verified by a PTY/`View()`-level assertion that the typed rune neither reached the composer nor the shell.

---

## 6. Acceptance-criteria map

| Criterion | Where it is satisfied |
|---|---|
| Pending write/exec renders tool + target with three arrow-key actions | steps 1–3 (item + `kit2.Card` + transcript dispatch) |
| Card owns the keys; typing does not leak; a bare letter does nothing | step 5 (`ClaimsKeys` at `router.go:729-733`) + step 6 tests |
| Escape denies; turn stays usable; refusal recorded | steps 7–8 (`DecisionDeny` + resolved record row + reconcile) |
| Session grant suppresses further cards for a directory; grants visible | steps 7 + 10 (grant scope + roll-up field/`/grants`) |
| Persistent list listable/addable/removable; visible in the GUI | step 11 (through the sibling's store = the file) |
| Denied entry says a grant cannot override it | step 9 |
| Clarifying card with options + "Other"; selection sent as the next message | step 12 (`SendUserMessage` → `app.go:4059`) |
| Wording matches the GUI's | step 13 (literal + cross-client assertions) |

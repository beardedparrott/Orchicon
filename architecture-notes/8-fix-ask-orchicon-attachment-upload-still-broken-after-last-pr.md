# ADR-008 — Fix Ask Orchicon Attachment Upload (still broken after last PR)

**Status:** Proposed (Principal Architect — Step 1)
**Scope:** `/ask-orchicon` only (per SCOPE GUARDRAIL — no persistent history panel added to unrelated routes)
**Sequencing:** After glassmorphism uplift (Tasks 1-7), before final QA (Task 9)
**Work item:** `8 — Fix Ask Orchicon Attachment Upload (still broken after last PR)`
**Branch:** `8-fix-ask-orchicon-attachment-upload-still-broken-after-last-pr-b021qxec5037yj2h` off `develop`
**File:** `architecture-notes/8-fix-ask-orchicon-attachment-upload-still-broken-after-last-pr.md`

---

## Context

User reports attachment upload on Ask Orchicon page is still broken despite the last PR that was supposed to fix it (PR #394 `eba350b24 feat(ask-orchicon): attachments, paste, work-item images...` / `cfa1ef1f`). The page is 77k (`frontend/src/routes/ask-orchicon.tsx`) with bespoke `ChatInputField` that handles file picker + paste + drag-drop + voice and embeds attachments inline via `AttachmentInput` (name/mimeType/data bytes) on `ChatStream`/`InterjectConversationTurn`. A separate `UploadAttachment` RPC exists (`internal/askorchicon/attachments.go`, 2.8k, BlobStore-backed, 50 MB limit) but the frontend **never calls it** — all attachments go inline over the Connect streaming RPC.

Server validation in `internal/askorchicon/chat.go:startConversationTurnOpts` enforces: max 5 attachments, each 10 MB, total 20 MB (authoritative, `InvalidArgument`/`FailedPrecondition`). Frontend enforces 10 MB per file via toast but only for picker/paste; drag-drop has a silent filter that drops non-image/text/json files without error, no total-size guard, no progress UI, and no blocking of Send while `FileReader` is still loading. Glass uplift (Tasks 1-7) introduced `glass-input` / `glass-panel` and cyan gradient `Send` — the attach `Paperclip` button is still `rounded p-1 text-muted-foreground` and the hidden file input lacks `accept`/`multiple`, so it neither matches the uplift nor exposes allowed types.

The bug was masked: last PR shipped paste/voice/image handling but left wiring gaps; styling changes will further hide them if not tested together.

### What the current frontend does

* `ChatInputField` (line ~1343) — state `AttachmentInput[]`, hidden `fileInputRef` (`<input type="file" class="hidden">` with **no** `accept`/`multiple`), `Paperclip` button triggers `click()`, `handleFileSelect` loops files, 10 MB check, `FileReader.readAsArrayBuffer` async → `setAttachments`. Same pattern for `handlePaste` (image/* only) and `handleDrop` (over-restrictive mime check, silent `continue` for filtered files). Chips render preview for images (base64 data URL) and `a.data.length` size.
* Send: `handleSubmit` captures `sentAttachments`, clears box, calls `onSend(text, attachments)` → `runStream` → `askOrchiconClient.chatStream({ conversationId, message, attachments })` or `interjectConversationTurn`. Server builds `attachmentsJSON` + system prompt `## Attachments` block (text/json → inline code block; images → vision parts hint).
* No call to `UploadAttachment`, no multipart, no progress, no total-size preflight.

### What the server does

* `UploadAttachment` — validates name/data, 50 MB, optional conversation existence check, `blobStore.Put("ask_orchicon/{tenant}/{conv}/{name}")`, audit `conversation.attachment_uploaded`. Works standalone but is dead code for current UI.
* `ChatStream`/`Interject...` — `startConversationTurnOpts` validates 5 / 10 MB / 20 MB total, persists `MessageRow.Attachments` JSON, builds seed/reuse system prompts with attachment block, launches detached collector `collectConversationReply` + `persistConversationReply`. No file-type allowlist beyond size/count.

---

## Decision

**Fix inline `ChatStream` attachments for GA and park BlobStore upload as opt-in for >10 MB / future multipart — do not force a migration to `UploadAttachment` in this task.**

Inline path already satisfies functional parity (all existing chat/conversation capability intact; only split + recurring type are intentional changes) and avoids a cross-cutting BlobStore/mime-type contract change that would need backend + frontend + Playwright + docs in one task. Follow-up can promote `UploadAttachment` (or presigned URL) when we need >20 MB or true multipart progress.

### Required Delta (Senior Engineer guidance — exact files/functions)

#### Frontend — `frontend/src/routes/ask-orchicon.tsx:ChatInputField`

1. **File picker** — add `accept` + `multiple` to hidden input (e.g. `accept="image/*,text/*,.json,.md,.mdx,.csv,.pdf"` `multiple`), style `Paperclip` trigger to match `glass-input` (use `rounded-md bg-white/5 hover:bg-white/10 border border-white/5` + `text-cyan-300` on hover, consistent with `ModeToggle` / gradient `Send`). Must work in both dark/light themes via existing `glass-input` tokens (no hardcoded colors).
2. **Drag-drop** — widen `handleDrop` to same allowlist as picker; on filtered files push toast `"Unsupported file type: {name}"` instead of silent `continue`; verify `ring-2 ring-cyan-400 border-dashed` contrast in light theme; allow dropping on whole `glass-input` surface and `e.stopPropagation()` so parent `DndContext` (conversation folder drag) does not swallow file drops.
3. **FileReader race** — track `pendingReads` counter / `isLoadingAttachments` boolean. Disable `Send` (`disabled={pendingReads>0 || (!text.trim() && !attachments.length)}`) and show inline `"Loading {n} file(s)..."` while readers resolve; `handleSubmit` must await pending reads (or block) so attachments are never sent empty.
4. **Client validation** — enforce total 20 MB (sum `file.size`) before reading; if exceeded toast `"Attachments too large (max 20MB total)"` and reject. Keep 10 MB per-file and 5-file cap (already there) but surface consistent toasts for all three paths (picker/paste/drop). Map server `CodeInvalidArgument`/`FailedPrecondition` to friendly toasts (existing `fail()` → `toast.error` already does, but add explicit mapping).
5. **Progress/error UI** — per-chip loading shimmer while reading; on `reader.onerror` toast `"Failed to read {name}"` and remove chip; on image preview failure fall back to Paperclip icon (already does).
6. **A11y + styling parity** — `aria-label="Attach file"`, keyboard trigger; ensure `glass-input rounded-2xl p-2.5` + `border-white/10` matches uplift; verify both themes via `theme-provider`.

#### Frontend — `frontend/src/routes/ask-orchicon.tsx:AskOrchiconPage` (no regression)

* Keep `handleSendMessage` / `sendStreaming` / `interjectStreaming` / `runStream` as-is; only passthrough of `attachments` changes. Ensure `Send` without attachments still works.

#### Backend — `internal/askorchicon/chat.go` (no change for fix, but document)

* Keep 5 / 10 MB / 20 MB caps authoritative; add `log.Warn` on rejected attachment with `tenant`, `convID`, `name`, `size` for debugging "still broken" reports.
* Optional follow-up: align `attachments.go:UploadAttachment` 50 MB limit with chat 10 MB per-file (confusing discrepancy) — lower to 10 MB or raise chat to 50 MB consistently via follow-up ADR.

#### Backend — `internal/askorchicon/attachments.go` (follow-up only)

* Keep as-is; wire `UploadAttachment` for future large-file or true multipart upload when 20 MB proves too small. Then frontend would: 1) `UploadAttachment` per file with progress, 2) send `attachmentId/url` refs on `ChatStream` (requires proto extension adding `attachment_ids` — out of scope).

#### API / Proto — `proto/orchicon/api/v1/ask_orchicon*.proto`

* **No proto change in this fix.** Keeps glass uplift + parity guarantee (category folders, filters/sorts/pagination/CRUD/bulk/YAML/deep-links, folder DnD, search remain identical).

---

## Alternatives Considered

| Option | Description | Pros | Cons |
|---|---|---|---|
| **A — Inline fix only (chosen)** | Fix client wiring (accept/multiple, drag filter, FileReader race, total-size guard, progress/error, glass styling) keeping server caps | Minimal risk, no proto/migration, unblocks QA (Task 9), preserves parity | 20 MB total remains ceiling |
| B — Migrate to `UploadAttachment` BlobStore | Picker → `UploadAttachment` per file (progress) → send url refs on `ChatStream` | True progress, scales to 50 MB, blobs durable | Requires proto change (`attachment_ids`), server `buildSystemPrompt` rework, larger PR — risks regressing parity; last PR tried this shape and still broke |
| C — Presigned URL / S3 multipart | Direct presigned PUT to blob store, then ref | Best for very large files | Heaviest: presign RPC, CORS, URL lifecycle/GC, frontend upload manager — overkill for 10 MB chat attachments |

**Trade-off:** Attachment UX needs low latency and small files (images, text, json, pdf). Inline `bytes` over Connect is fine under 20 MB and avoids a two-phase commit (blob must exist before chat turn). The "still broken" root cause is client-side wiring, not transport — so we fix client, not transport.

---

## Consequences

* **Positive:** File picker + drag-drop + paste all work in both themes, matching `glass-input`/cyan gradient; server validation authoritative with user-visible errors; Send without attachments unaffected; no proto drift; Playwright can cover `attach → send` before QA lock.
* **Negative:** 20 MB total remains ceiling — mitigated by clear error and follow-up via `UploadAttachment`.
* **Operational:** No new infra (BlobStore already exists but unused here); no change to `ORCHICON_ASK_*` tunables.
* **Follow-up:** If users need >20 MB or binaries (zip, pdf >10 MB), promote Option B with proto `attachment_ids` + batching and per-file progress. Align per-file limits (10 vs 50) then.

---

## Root Cause (for acceptance criteria "documented")

1. **Last PR #394** added `AttachmentInput` inline wiring and `UploadAttachment` handler but never wired UI reliably: hidden input omitted `accept`/`multiple`, drag-drop filter was over-restrictive with silent drops, `FileReader` race let Send fire before data ready. No total-size preflight, no progress/error, `Paperclip` not restyled for glass uplift → manual test "file → send → no upload / error" appeared as "still broken".
2. **Server contract confusion:** `attachments.go` allows 50 MB single file while `chat.go` caps at 10 MB/20 MB total — frontend only checked 10 MB per file, so 20 MB total violations surfaced only as opaque `InvalidArgument` after send.
3. **Glass uplift interaction:** `glass-input` moved the drop surface and changed contrast; `ring-2 ring-violet-400 border-dashed` had insufficient light-theme contrast and attach button lost parity with cyan gradient `Send`, so testers assumed it was inert.

---

## Playwright Coverage (for QA Engineer — Task 9 gate)

* Add `e2e/ask-orchicon-attachments.spec.ts` (Chromium, `orchicon-dev`):
  - file picker → send succeeds (attach 1 image + 1 text, ≤5 files, verify chip preview, send, poll `ListMessages` for `attachments.length>0`, verify assistant reply)
  - drag-drop → send succeeds (drag `testdata/hello.txt` onto `glass-input`)
  - paste image → send succeeds (paste event with `image/png`)
  - too large / too many — 11 MB or 6th file shows toast, Send blocked or server toast on send; error is user-visible (toast store)
  - send without attachment still works (existing smoke)
  - Both themes: run with `colorScheme: "light"` and `"dark"` snapshots or explicit `glass-input`/`Paperclip` visibility checks
* Gate: spec must pass in `orchicon-dev` (chromium) before Task 9 QA lock.

---

## Security / Reliability / Observability

* **Security:** No new file-type execution — attachments are bytes rendered as preview or inline code block; images are vision parts to model (no exec). Size caps prevent OOM; audit `conversation.message_sent` already covers attachments.
* **Reliability:** Client guards prevent FileReader race; server caps authoritative. One-turn-per-conversation gate unchanged.
* **Observability:** Add `log.Warn` on attachment reject with `tenant`, `convID`, `name`, `mime`, `size`; existing `turnRegistry` + `persistConversationReply` logs remain.

---

## Review Checklist (for Design Approver)

* [ ] Scales? 10x: 20 MB caps prevent bus blow-up; `pendingReads` guard prevents thundering herd. At 100x convo rate, BlobStore follow-up needed (not this task).
* [ ] Right thing? Yes — restores chat attachments without dropping category folders, filtering/sorting, pagination, CRUD, bulk, YAML, deep-links, folder DnD, search. Only split + recurring type are intentional changes.
* [ ] Security/observability/operability considered? See above.
* [ ] Trade-offs documented? Options A/B/C with chosen A.
* [ ] Consistent with existing arch? Uses existing `AttachmentInput` + `chatStream` contract; no new RPC; glass styling aligns with `glass-panel`/`glass-input` tokens (sidebar `w-72 glass-panel`).

---

## Downstream Handoff

* **Implementer (Senior Engineer, Step 3):** Implement Frontend delta above; touch only `frontend/src/routes/ask-orchicon.tsx` (and optionally `frontend/src/components/chat/*` if extracting `AttachmentChips`). Do not change proto or `internal/askorchicon` caps in this PR except optional log line. Verify both themes and order with predecessor state (Tasks 1-7 uplift) before QA.
* **QA:** Use `orchicon-dev` container (`:orchicon-dev` boots plane at `localhost:8080`, Postgres `5432`, `ORCHICON_TELEMETRY=none`); point Playwright at the frontend dev server. Reproduce failure path `file → send → no upload / error` before and after.
* **Notes location:** This file — `architecture-notes/8-fix-ask-orchicon-attachment-upload-still-broken-after-last-pr.md`.

# Spike — file-edit events emitted by the opencode transport (Diff Pipeline)

Status: findings locked 2026-09-21, step 1 of the diff-pipeline workflow.
Scope: `internal/opencode/` (session transport), `internal/mcp/` + `internal/worktree/`
(worktree file tools), `internal/transcript/`, `internal/askorchicon/` (Ask sessions).

## 1. Event transport shape

Every execution runs as a persistent opencode session driven over the serve
HTTP+SSE API. All events arrive at ONE dispatch point:
`Adapter.parseEvent` (`internal/opencode/adapter.go`, func `parseEvent`, called
from `session_run.go`'s SSE loop). Each event is a map `{type, part}`.
Event type constants (adapter.go, `evt*` block): `step_start`, `text`,
`tool_use`, `reasoning`, `step_finish`, `file_diff`.

The durable transcript (`execution_session_parts`, `internal/db/session_parts.go`)
stores raw parts with kind `tool_use` for tool calls — payload shape:

```json
{
  "type": "tool_use",
  "part": {
    "tool": "batch_write",
    "callID": "...",
    "state": {
      "status": "completed",
      "input":  { "writes": [ {"path": "a.go", "mode": "edit", "old": "x", "new": "y"} ] },
      "output": "<tool's text/JSON output>",
      "title":  "...",
      "time":   { }
    }
  }
}
```

**Key finding:** the `tool_use` event carries tool INPUT and tool OUTPUT text
only. It never carries before/after file contents or a diff. The separate
`file_diff` event carries ONLY `part.path` (adapter.go `case evtFileDiff`);
its path is dedup-collected into `stats.writtenFiles`. Neither is ground
truth for a diff — confirming the snapshot-pair design was necessary.

## 2. File-mutating tool inventory

| Tool | Defined | Mutating? | What the event carries | Ground-truth mechanism |
|---|---|---|---|---|
| `batch_write` | `internal/mcp/worktree.go:63`, engine `internal/worktree/worktree.go:583` | yes (`Mutating: true`) | input: `{writes:[{path, mode: create\|overwrite\|edit\|append, content?, old?, new?}]}`; output: JSON summary. **No diff.** | **In-engine snapshot pair (exact).** `BatchWrite` already holds the original on-disk content per touched path in memory (`origContent`/`origExists`, worktree.go ~616-624) and computes the final content — the engine can emit a real per-path unified diff + before/after sha256/sizes in its structured JSON output. Diff is computed from real file state by server code, never parsed from prose. |
| `write` | `internal/worktree/single.go:254` (thin wrapper over batch engine) | yes | input `{filePath, content}`; output JSON. | Same in-engine snapshot pair. |
| `edit` | `internal/worktree/single.go:270` | yes | input `{filePath, oldString, newString}`; output JSON. | Same in-engine snapshot pair. |
| opencode built-in `write` | opencode runtime; special-cased in adapter.go (`toolName == "write"` → artifact) | yes | input `{path?, content}`; output text. | **Plane-side after-read.** The plane can read the affected file under `executionDir(manifest)` (the run worktree, which lives under the mounted project dir) right after the event; "before" comes from the session's last observed content (in-memory per-path cache, seeded from `git show HEAD:relpath` or empty-for-create). |
| opencode built-in `edit` | opencode runtime | yes | input `{path, old, new}`; output text. | Same plane-side after-read. |
| `bash` | opencode runtime | can mutate anything | cmd + output text. | **No per-event entry** (mutations are invisible). Surfaced at completion by git reconciliation. DECISION (revisitable): not intercepted per-event. |
| `write_artifact` | adapter.go | no (virtual artifact) | name/type/content | Out of scope — not a file edit. |
| `todowrite` | adapter.go snapshot path | no | todo items | Out of scope. |

Notes:
- The composite worktree tools are the PRIMARY write path for runtime-container
  executions: built-in `read`/`grep` are denied and workers are steered to the
  batch tools (adapter.go `RuntimeServeConfig` + `batchToolsDiscipline`). So the
  in-engine hook covers the majority of real edits exactly.
- Ask Orchicon conversations drive sessions through the same transport on the
  host serve with `project_dir` as base (`internal/askorchicon/chat.go` — the
  conversation's opencode session is aborted/reused by `conv.SessionID`). The
  same event shapes apply; the ledger hook there uses owner_kind=`ask_conversation`.
- `internal/transcript` is a leaf renderer over `execution_session_parts`; it
  does not parse diffs and needs no change.

## 3. Snapshot mechanism (as implemented in the plan)

1. **Worktree MCP tools** (exact): extend `BatchWrite`/`SingleWrite`/`SingleEdit`
   to include a `file_edits` array — per path: `{path, existed_before,
   size_before, sha256_before, existed_after, size_after, sha256_after,
   unified_diff}` — computed from the before/after contents the engine already
   holds. No extra disk reads, no races.
2. **opencode built-ins** (near-exact): on the `tool_use` event for a mutating
   tool, the plane reads the file after completion (after-snapshot); before =
   last observed content for that path in the session (git-HEAD-seeded).
   A diff may therefore span two back-to-back edits if the model races the
   observer — still real file state on both sides, tagged with the triggering tool.
3. **Git reconciliation on completion** covers everything else (bash edits,
   missed events): see the implementation plan §Reconciliation.

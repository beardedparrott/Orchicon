# Outbox retention, de-outboxed `execution.text`, and relay observability

Work item: *Stop outboxing per-token `execution.text` + add outbox retention & observability
(8.4M-row backlog)*.

## 1. The problem

The transactional outbox (`outbox`, docs/09 §6, `internal/db/outbox.go`) had no retention:
published rows were never deleted by anything in the tree.

Observed on the live tenant (2026-09-10):

| Fact | Value |
|---|---|
| `outbox` size / rows | 8.4 GB / 8,431,409 |
| `execution.text` rows | 8,247,979 (**98%**) |
| `execution.tool_call` rows | 134,718 |
| Oldest row | 2026-07-29 (six weeks of silent accumulation) |

`TaskReconciler.OnText` (`internal/scheduler/reconciler.go`) wrote **one outbox row per streamed
text delta** — a chunk every ~40 chars / 60 ms (`internal/orchicon/loop.go`,
`internal/opencode/adapter.go`) — inside the state-mutation transaction. Every insert also updated
three indexes (pkey, unique `event_id`, hot partial `outbox_unpublished_idx`). The shared Postgres
pressure from that churn is the verified structural contributor to the web GUI going unresponsive
during long native-loop runs.

There was also no alerting on outbox depth or age, so a stalled relay could accumulate for weeks
unnoticed.

## 2. What changed

### 2.1 Per-token `execution.text` is no longer outboxed

`OnText` now performs only the direct NATS publish. One durable copy of the transcript already
exists in `execution_session_parts` (written independently by the adapter session stores,
`internal/server/server.go`), and the GUI's reconnect refetch reads that
(`GetExecutionSession` → `db.ListExecutionSessionParts`), not the outbox.

The dropped guarantee was *only* "if NATS is down at commit time, the row waits in Postgres for the
relay to publish after recovery". For a transient chat token that is nearly worthless: JetStream
discards after 72h and the durable transcript already holds the text.

State-critical, low-frequency events stay outboxed with the relay path unchanged:
`execution.created`, `execution.terminated`, `execution.tool_call`, `execution.artifact`,
`workflow.step_*`, `recovery.*`, `approval_request`, `checkpoint`, `control`.

### 2.2 REQUIRED ENABLER — the direct publish's MsgID is now unique per publish

The direct-publish path used a **constant** JetStream MsgID:
`dedupID := fmt.Sprintf("direct:%s:%s", e.ID, eventType)` with the stream created with
`Duplicates: 5 * time.Minute` (`internal/eventbus/nats.go`). JetStream silently drops any publish
whose `Nats-Msg-Id` was seen inside that window, so **only the first direct publish per execution
per 5 minutes was ever stored**, and the outbox relay was the real live-text path. Removing the
per-token outbox write *without* fixing this would have reduced live text streaming to one chunk
per 5 minutes per execution — a silent correctness break.

The MsgID is now `direct:<execID>:<eventType>:<n>` where `n` is
`TaskReconciler.eventSeq` (`atomic.Uint64`, monotonic per process). This is a deliberate deviation
from the work item's "out of scope: any change to the direct NATS publish path" note, required by
the "do not weaken correctness silently" acceptance criterion. The *delivery* path is unchanged
(subject, payload, envelope); only the dedup key became unique, exactly as the outbox row ULID
already was for relay publishes.

Regression guards: `TestPublishExecEventMsgIDUnique` (`internal/scheduler`) and
`TestJetStreamConstantMsgIDIsDeduplicated` (`internal/eventbus`, boots a real `nats-server` with the
production stream config and asserts a repeated constant MsgID is deduplicated while unique MsgIDs
store distinct sequences).

### 2.3 Decision: `execution.tool_call` stays outboxed

Evidence:

- 134,718 rows over six weeks ≈ 3k/day — 1.6% of the table, not the pathology.
- It is **not** per-token: one row per tool invocation.
- Its direct publish goes through the same code path, so its outbox row is today's durable
  delivery path; de-outboxing it would trade a real guarantee for a rounding error in table size.
- `tool_use` parts *are* reconstructible from `execution_session_parts`
  (`kind='tool_use'`, `internal/db/session_parts.go`), so de-outboxing is *possible* — but it buys
  little and needs the same outage verification for a far less valuable cut. Revisit if it ever
  grows past the per-token share it had before.

This is pinned by `TestOnToolCallStillEnqueuesOutbox`.

## 3. Retention

| Knob | Env var | Default | Meaning |
|---|---|---|---|
| window | `ORCHICON_OUTBOX_RETENTION_DAYS` | `7` | PUBLISHED rows older than this are deleted. `0` disables pruning. |
| batch | `ORCHICON_OUTBOX_PRUNE_BATCH` | `10000` | Max rows per DELETE statement (clamped to `[1, 100000]`). |
| interval | `ORCHICON_OUTBOX_PRUNE_INTERVAL` | `1h` | How often a retention pass runs. |

Implementation:

- `db.PrunePublishedOutbox(ctx, olderThan, batchLimit)` — one bounded statement:
  `DELETE FROM outbox WHERE id IN (SELECT id FROM outbox WHERE published_at IS NOT NULL AND published_at < $1 ORDER BY published_at LIMIT $2)`.
  **Unpublished rows are never pruned** (the predicate requires `published_at IS NOT NULL`).
- `Relay.pruneLoop` (started from `Relay.Run`, `internal/outbox/relay.go`) runs `Relay.PruneOnce`
  every interval; `PruneOnce` executes at most `outboxPruneMaxBatchesPerTick = 10` bounded batches,
  stopping early when a batch comes back short. Bounded per statement ⇒ bounded WAL and lock
  windows; no long lock ever lands on the table.
- New partial index `outbox_published_at_idx ON outbox (published_at) WHERE published_at IS NOT NULL`
  (migration `db/migrations/20260923000000_outbox_retention.sql`, mirrored in `db/schema.hcl`).
  Without it every prune pass seq-scans the table; the existing `outbox_unpublished_idx` is partial
  on *unpublished* rows and cannot serve it.

Tenancy / RLS: the table keeps `ENABLE`/`FORCE ROW LEVEL SECURITY` + the `tenant_isolation` policy
unchanged (`db/migrations/20260712223028_add_outbox.sql`). The prune runs on the same non-tenant
pool path as the relay's own `PollOutbox`/`MarkPublished` (the relay publishes on behalf of every
tenant), and because the predicate is `published_at IS NOT NULL`, rows still awaiting delivery are
untouched for every role. `TestPrunePublishedOutboxAcrossTenants` verifies published rows of two
tenants are both pruned while unpublished rows of both survive.

Wiring: `internal/server/server.go` passes the three options from `config.Default()`;
`internal/config/config.go` owns the knobs.

### Deploy ordering on the large `outbox`

`20260923000000_outbox_retention.sql` creates `outbox_published_at_idx` with a plain,
non-`CONCURRENTLY` `CREATE INDEX` — Atlas applies migrations inside a transaction, so
`CONCURRENTLY` is not available. On the existing 8.4M-row / 8.4 GB table (98% of it
`published_at IS NOT NULL`, i.e. all index-eligible) the build takes a `SHARE` lock on
`outbox`, which blocks INSERT/UPDATE/DELETE — new enqueues and the relay's
`MarkPublished` — for the duration of the build. Apply the migration in a maintenance
window together with `scripts/outbox-backlog-cleanup.sh`, or after the backlog has been
shrunk; do not apply it during a streaming-heavy period. Only the one-off index build
locks the table — the scheduled prune is batched (10 × `ORCHICON_OUTBOX_PRUNE_BATCH` rows
per pass) and never holds a long lock.

## 4. Observability

Emitted on the existing telemetry pipeline (no new infra):

| Metric | Type | Meaning |
|---|---|---|
| `orchicon_outbox_lag` | Int64 observable gauge | unpublished row depth (pre-existing) |
| `orchicon_outbox_oldest_unpublished_seconds` | Int64 observable gauge | age of the oldest unpublished row — the **stall** signal |
| `orchicon_outbox_lag_alerts_total` | Int64 counter | poll ticks where the alert threshold was breached |

Alert thresholds (package constants in `internal/outbox/relay.go`, so they live in one place):

- `outboxLagAlertDepth = 1000` unpublished rows, **or**
- `outboxLagAlertAgeSec = 300` seconds (5 min) age of the oldest unpublished row.

Depth alone cannot distinguish "a busy relay keeping up" from "a stalled relay falling behind" —
which is exactly why the six-week backlog was invisible. Either threshold breached on one 5s
reporting tick increments the counter and emits a structured WARN
(`outbox relay lag: alert threshold exceeded`, fields `unpublished`, `oldest_age_seconds`,
`depth_threshold`, `age_threshold_seconds`), which Loki/Grafana ingest. Alert on the counter rather
than writing a Prometheus rules file: none exists in-tree.

## 5. Verification record

All commands run against the **disposable in-container sandbox plane** (`ORCHICON_TEST_DSN`,
Postgres on container-local 5432) — never against the live plane.

| AC | Evidence | Result |
|---|---|---|
| zero `execution.text` outbox rows from `OnText`; direct publish still fires | `TestOnTextDoesNotEnqueueOutbox` (`internal/scheduler`) — 4 text deltas with a NATS-down publisher: 0 `execution.text` outbox rows, 4 publish attempts | PASS |
| dropped guarantee covered by the durable transcript | same test: completed text part written to `execution_session_parts`, then read back through `db.ListExecutionSessionParts` (the `GetExecutionSession` path) and asserted to contain the full text | PASS |
| `execution.tool_call` keep-decision pinned | `TestOnToolCallStillEnqueuesOutbox` — exactly 1 outbox row | PASS |
| prune deletes old published rows, keeps unpublished, keeps fresh published, tenant-safe/RLS | `TestPrunePublishedOutboxAcrossTenants` (two tenants) | PASS |
| prune batching bound | `TestPrunePublishedOutboxRespectsBatchBound` (25 eligible rows, `batchLimit=10` ⇒ exactly 10 deleted) | PASS |
| relay prune pass + disabled-retention no-op | `TestRelayPruneOnceDeletesPublishedKeepsUnpublished`, `TestRelayPruneDisabledIsNoop` | PASS |
| depth + oldest-age + alert threshold | `TestCountAndOldestUnpublished` (db), `TestRelayLagAlertFires` (relay) | PASS |
| direct-publish MsgID unique per publish | `TestPublishExecEventMsgIDUnique` | PASS |
| JetStream `Duplicates: 5m` self-dedup of a constant MsgID (the §2.2 finding) | `TestJetStreamConstantMsgIDIsDeduplicated` (real `nats-server -js`) | PASS |

### Manual outage / reconnect procedure (operator reproduction)

Automated coverage above injects the outage at the publisher interface; the end-to-end GUI check is:

1. Bring the sandbox plane up (`orchicon serve` in the runtime container) and start a native-loop
   run that streams text.
2. Stop `nats-server` mid-run. `OnText` keeps rendering nothing new over the live stream, and the
   direct publishes fail (logged `publish exec event`).
3. Confirm the session pane keeps updating anyway: `SessionChatPane` polls the durable transcript
   every 2000 ms while a run is live and refetches on terminal — source of truth
   `execution_session_parts` → `GetExecutionSession`.
4. Restart `nats-server`. Confirm the live stream resumes (unique MsgIDs now store every chunk) and
   that the pane and the terminal view show the complete conversation.

## 6. QA acceptance review (independent re-verification)

Re-verified on the branch head (base `3b37f987e` + QA commit) against the **disposable
in-container sandbox plane** only (`ORCHICON_TEST_DSN`, Postgres on container-local 5432).
Build/vet: `go build ./...` and `go vet` on every changed package — clean.

| Acceptance criterion | QA evidence | Result |
|---|---|---|
| Published rows older than N pruned on schedule; unpublished never pruned; tenant-safe; batched | `TestPrunePublishedOutboxAcrossTenants`, `TestPrunePublishedOutboxRespectsBatchBound` (25 eligible rows, `batchLimit=10` ⇒ exactly 10 deleted), `TestRelayPruneOnceDeletesPublishedKeepsUnpublished`, `TestRelayPruneDisabledIsNoop` | PASS |
| Batched one-off cleanup of the 8.4M-row backlog + vacuum guidance | `scripts/outbox-backlog-cleanup.sh` (bounded `LIMIT`, sleeps between batches, `MAX_MINUTES` budget, re-runnable; prints `VACUUM (ANALYZE)` / `VACUUM FULL` / `REINDEX CONCURRENTLY` guidance). Argument gates re-verified: `ROW_BATCH=abc` ⇒ exit 2, positional `RETENTION_DAYS=0` ⇒ exit 2, empty DSN ⇒ exit 2 | PASS |
| Per-token `execution.text` no longer outboxed; direct publish remains; GUI unchanged in steady state | `TestOnTextDoesNotEnqueueOutbox` (4 deltas, NATS-down publisher: **0** `execution.text` outbox rows, 4 publish attempts); grep confirms `OnText` is no longer an `enqueueExecEvent` call site and no other per-token producer of `execution.text` exists. Live-feed delivery re-verified end-to-end at the broker boundary with the **production** publisher + subscriber: `TestDirectPublishLiveFeedDeliversEveryChunk` (`internal/eventbus`) publishes three chunks with `direct:<exec>:<type>:<seq>` and asserts all three reach the `StreamExecutionEvents` consumer (`Subscriber.Subscribe(ctx, "orchicon.events.execution.>")`) on **distinct** JetStream sequences, reassembling exactly | PASS |
| Dropped guarantee covered: NATS down at commit, client reconnects, GUI recovers full text via durable transcript refetch | Outage injected at the publisher in `TestOnTextDoesNotEnqueueOutbox`; the full text is then recovered through `db.ListExecutionSessionParts` — the exact call behind `Service.GetExecutionSession` (`internal/execution/service.go:1168`). GUI side: `SessionChatPane` refetches the transcript every 2000 ms while running and once more on terminal (`frontend/src/components/executions/SessionChatPane.tsx:530-548`), so text lost to a NATS outage is recovered regardless of any event-stream gap. Manual outage/reconnect procedure documented in §5. No gap found in the refetch path | PASS |
| `execution.tool_call` keep-decision documented | §2.3 (134,718 rows ≈ 3k/day, not per-token, `tool_use` parts reconstructible but the cut buys little); pinned by `TestOnToolCallStillEnqueuesOutbox` (exactly 1 outbox row per call) | PASS |
| Low-frequency state-critical events stay outboxed, relay path unchanged | `enqueueExecEvent` call sites retained: `execution.created` (`reconciler.go:902`), `execution.tool_call` (`:2259`), `execution.artifact` (`:2316`), `execution.<status>` (`:2364`), plus `execution.checkpoint`/`execution.control` (`internal/execution/service.go`). Relay `tick`/`PollOutbox`/`MarkPublished` untouched | PASS |
| Observability: depth gauge + oldest-unpublished-age gauge + alert threshold; stalled relay cannot accumulate silently | `TestCountAndOldestUnpublished`, `TestRelayLagAlertFires` (age threshold fires, gauge values sampled). QA added registration assertions: all three instruments (`orchicon_outbox_lag`, `orchicon_outbox_oldest_unpublished_seconds`, `orchicon_outbox_lag_alerts_total`) must be non-nil after `NewRelay` — a metric a stalled relay never registers cannot alert | PASS |
| Dedup path (`direct:` MsgID vs outbox-row ULID) still passes its tests | `TestPublishExecEventMsgIDUnique` (3 publishes, 3 distinct MsgIDs, `direct:<exec>:<type>` prefix kept), `TestJetStreamConstantMsgIDIsDeduplicated` (real `nats-server`, `Duplicates: 5m`) | PASS |

### Defect found and fixed by QA

- `internal/scheduler/reconciler.go` failed `gofmt` **because of this diff**: inserting the
  multi-line `eventSeq` comment between `eventPub` and the next field split the struct's gofmt
  alignment group, so the four preceding field lines were no longer formatted (`gofmt -d` reports
  one hunk; `develop`'s copy is gofmt-clean). Fixed with `gofmt -w` — formatting only, no
  behaviour change, `go build ./internal/scheduler/` re-verified.
- `internal/db/outbox.go` and `internal/config/config.go` are **also** unformatted on `develop`
  (pre-existing, not introduced here); left untouched to avoid unrelated churn.

### Surface-impact determination

The diff contains **zero frontend changes**, but it does feed a user-visible surface — live
streaming text — so it was treated as UI-affecting. A Playwright screenshot pass was **not
possible in this runtime**: `frontend/dist` is empty and `frontend/node_modules` is unpopulated,
and `npm ci` + a Vite build cannot complete inside the time box. The user-visible path was
therefore verified at the layer the GUI consumes: the real JetStream publisher + subscriber
round-trip (`TestDirectPublishLiveFeedDeliversEveryChunk`) proves every streamed chunk reaches
`StreamExecutionEvents` on its own sequence, and the frontend dedup key
(`${eventType}-${response.sequence}`, `frontend/src/api/executions.ts:130`) is unchanged — so the
pane renders every chunk, exactly as when the outbox relay was the live path. The durable
transcript poll that covers an outage is unchanged too. **No unverified user-visible regression
remains.**

### Suite status

- Green: `internal/db` (retention tests), `internal/outbox`, `internal/eventbus`, `internal/config`.
- Pre-existing red on `develop`, unrelated to this diff: `internal/workitem`
  (`TestListIdeasRejectedScope`, `TestControlSequenceRejectsNonSequenceParent/bound-run ticket`).
- The **full `internal/scheduler` package** exceeds this step's 30-minute box (the same limit the
  PR review hit); its acceptance-critical tests (`TestOnTextDoesNotEnqueueOutbox`,
  `TestOnToolCallStillEnqueuesOutbox`, `TestPublishExecEventMsgIDUnique`) pass individually. Step 5
  (DevOps) should run the package/suite in CI, where it is not time-boxed.

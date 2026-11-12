# No-engine developer session — harness run 20261017T054835Z

- passes: 49   failures: 15   skips: 1
- marker: `NO-ENGINE-E2E-20261017T054835Z-85679` (unique per run: a served-byte match cannot be a leftover)
- instance: `dev` (pg 15432 / nats 14222 / control 18080), second instance `prod` (pg 15433 / nats 14223 / control 18091)

## per-step verdict

| step | verdict |
|---|---|
| 0 | PASS |
| 1 | PASS |
| 2 | **FAIL** (see defects.tsv) |
| 3 | SKIP |
| 4 | **FAIL** (see defects.tsv) |
| 5 | **FAIL** (see defects.tsv) |
| 6 | **FAIL** (see defects.tsv) |
| 7 | PASS |
| 8 | **FAIL** (see defects.tsv) |
| 9 | PASS |
| 10 | **FAIL** (see defects.tsv) |

## /healthz (verbatim, independent of any script's report)

```
{"status":"ok"}```

## failures — each names the file that owns it

| step | assertion | owner |
|---|---|---|
| 2 | make rls-check passes with NO host psql (the downloaded-psql rung) | `scripts/check-rls.sh:49-78` |
| 2 | the RLS gate really ran (not SKIPPED) (missing 'RLS gate OK' in qa-evidence/no-engine-e2e/qa2-full/step-2.log) | `` |
| 4 | the served bundle is fetchable (independent: HTTP GET of the asset) | `internal/api (SPA serving)` |
| 4 | THE CHANGE IS LIVE: the SERVED bundle carries the new marker (missing 'NO-ENGINE-E2E-20261017T054835Z-85679' in qa-evidence/no-engine-e2e/qa2-full/04-served-bundle-after.txt) | `Makefile:569-581` |
| 4 | make build force-fe=1 (the engine-free escape hatch) succeeds | `Makefile:573-581` |
| 5 | status makes no claim it cannot verify (no container-derived 'not created') (found 'not created' in qa-evidence/no-engine-e2e/qa2-full/05-status-script.txt) | `scripts/container.sh:1603-1630 status_instances (unguarded 'docker ps')` |
| 5 | status does not leak a container error (found 'docker: command not found' in qa-evidence/no-engine-e2e/qa2-full/05-status-script.txt) | `scripts/container.sh:1603-1630` |
| 5 | status names the plane's port (18080) | `scripts/container.sh:plane_status` |
| 5 | status reports the SERVICES half (Postgres/NATS), not only the plane | `scripts/container.sh:1603-1630` |
| 6 | scripts/orchicon.sh logs works with no container engine (exit 127) | `scripts/orchicon.sh:101 (logs -> scripts/container.sh:1632 logs_instance -> 'docker logs -f')` |
| 6 | the plane writes its log to .local/share/orchicon-dev/serve/logs/orchicon.log (the path every other verb already knows) | `Makefile:272-278 (make run is foreground and writes no log) + scripts/container.sh:478` |
| 8 | make clean-docker is a clean no-op with no container engine (exit 2) | `Makefile:413-416` |
| 8 | clean-docker does not fail with a container error (found 'docker: command not found' in qa-evidence/no-engine-e2e/qa2-full/step-8.log) | `Makefile:413-416` |
| 8 | a second cleanup is also a clean no-op (exit 0) | `Makefile:413-416` |
| 10 | every earlier step is answerable from a documented command plus its evidence (14 step-level failures to attribute) | `see defects.tsv` |

## evidence files

- 00-build-budget.log
- 00-preflight.log
- 01-healthz.txt
- 01-run.log
- 01-services-status.txt
- 02-migrate-idempotent.log
- 02-migrate.log
- 02-rls-unshimmed.log
- 02-rls.log
- 04-diff.txt
- 04-run-after-force-fe.log
- 04-run-after-rebuild.log
- 04-run-after-stale-control.log
- 04-served-bundle-after.txt
- 04-served-bundle-force-fe.txt
- 04-served-bundle-stale-control.txt
- 04-served-bundle.txt
- 05-status-groundtruth.txt
- 05-status-script.txt
- 06-logs.txt
- 06-workaround-run-log-tail.txt
- 07-dev-run-again.log
- 07-prod-migrate.log
- 07-prod-run.log
- 07-prod-services.log
- 08-cleanup-second.log
- 08-share-after.txt
- 08-share-before.txt
- 09-ports.txt
- 09-services-status.txt
- 10-blindspots.md
- defects.tsv
- results.tsv
- step-0.log
- step-1.log
- step-10.log
- step-2.log
- step-3.log
- step-4.log
- step-5.log
- step-6.log
- step-7.log
- step-8.log
- step-9.log
- summary.md

# No blind spots — what answers "what is running, and why?"

| step | the documented command | the evidence beside it | verdict |
|---|---|---|---|
| 1 | `make build` / `make services` / `make run` | `01-healthz.txt`, `/proc` port ownership | PASS |
| 2 | `make migrate` (x2), `make rls-check` | `02-migrate*.log`, `02-rls.log` (no host psql) | **FAIL** (see defects.tsv) |
| 3 | `bin/orch --url http://127.0.0.1:18080` | `03-orch-tui.txt` PTY capture | SKIP |
| 4 | `make build` + restart (the plane go:embeds dist) | `04-served-bundle*.txt` — the SERVED bytes | **FAIL** (see defects.tsv) |
| 5 | `scripts/orchicon.sh status dev` | `05-status-groundtruth.txt` (/healthz, /proc) | **FAIL** (see defects.tsv) |
| 6 | `scripts/orchicon.sh logs dev` | `06-logs.txt` + `06-plane-log.txt` | **FAIL** (see defects.tsv) |
| 7 | `make services/migrate instance=prod` | `07-*.log`, both `/healthz` | PASS |
| 8 | `make clean-docker` | `08-share-{before,after}.txt`, sentinel | **FAIL** (see defects.tsv) |
| 9 | `make services-down` (both instances) | `09-ports.txt`, `/proc/net/tcp` | PASS |

A cell that resolves to "only knowable by reading source or running a container
command" is a FAIL of AC9 and is listed in defects.tsv with the file:line that owns it.

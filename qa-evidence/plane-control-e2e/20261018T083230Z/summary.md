# Plane control across TUI / desktop app / CLI — harness run 20261018T083230Z

- instance: `dev` (data dir `/home/beardedparrott/.local/share/orchicon-dev`, ports `none recorded`)
- two-instance pair: `dev` + `prod`
- binary under test: `/tmp/orchicon/plane-control-e2e/install/orchicon` (a copy in a scratch install dir, never the worktree's build)
- passes: 5   failures: 0   skips: 1   blocked: 13

**NOT A VALID PROOF:** instance 'dev' is not installed on this host: the lifecycle steps cannot be executed here

## per-step verdict

| step | what it proves | verdict |
|---|---|---|
| 0 | validity precondition (installed, ports free, surfaces present) | BLOCKED (see defects.tsv) |
| 1 | TUI stop is really a stop (AC1) | BLOCKED (see defects.tsv) |
| 2 | desktop start brings it up (AC2) | BLOCKED (see defects.tsv) |
| 3 | reverse direction, one shared mechanism (AC3) | BLOCKED (see defects.tsv) |
| 4 | CLI parity (AC4) | BLOCKED (see defects.tsv) |
| 5 | drift reported on plane status (AC5) | BLOCKED (see defects.tsv) |
| 6 | fail-safe: left alone + foreign holder reported (AC6) | BLOCKED (see defects.tsv) |
| 7 | two instances isolated (AC7) | BLOCKED (see defects.tsv) |
| 8 | cross-instance environment stripped (AC8) | BLOCKED (see defects.tsv) |
| 9 | web app gained nothing (AC9) | PARTIAL (automated assertions passed, a manual observation is outstanding) |

## evidence (independent of any control's own report)

- `/healthz` codes, curl exit codes and the listening sockets are read from
  `/proc/net/tcp{,6}`; the serving binary from `/proc/<pid>/exe`; the PID from the
  instance's own PID file; the ports from its own `ports.json`.
- per-step logs: `NN-*.log` next to this file, each holding the exact commands,
  their exit codes and their verbatim output.

## defects — each names the sibling task that owns the fix (AC12)

| step | assertion | owner |
|---|---|---|
| 0 | A0.5 a TUI lifecycle surface exists (internal/tui or cmd/orch calls internal/planecontrol) [BLOCKED: grep 'internal/planecontrol' over internal/tui and cmd/orch matches nothing; cmd/orch/main.go:3 says the TUI carries zero control-plane] | `TUI task` |
| 0 | A0.6 a Wails desktop app exists [BLOCKED: no *.go file in the tree references wails: there is no desktop/ package, no wails.json and no packaged app to launch] | `Wails desktop app task` |

## evidence files

- 0-preflight.log
- 1-tui-stop.log
- 2-desktop-start.log
- 3-reverse-desktop-stop-tui-start.log
- 4-cli-parity.log
- 5-drift.log
- 6-failsafe.log
- 7-two-instances.log
- 8-env-leak.log
- 9-webapp.log
- defects.tsv
- results.tsv
- summary.md

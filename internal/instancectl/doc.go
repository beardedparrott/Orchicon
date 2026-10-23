// Package instancectl is the ONE operations contract for an Orchicon instance: list, status, install,
// uninstall, start, stop, restart.
//
// WHY IT EXISTS. The desktop app and the TUI are both "Servers" UIs (design of record, D1): a person
// managing Orchicon on their own machine should see the same servers, the same verbs and the same
// outcomes whichever client they open. Before this package the two drove DIFFERENT code — the desktop
// through internal/planebind, the TUI through internal/planecontrol directly — which is how the product
// came to print "✓ dev is stopped" over a live postgres still holding port 5432. Two implementations of
// one verb can disagree; one cannot.
//
// WHY IT DOES NOT EMBED THE INSTALL ORCHESTRATION. The obvious shape was to move install.go,
// uninstall.go, quiesce.go and ~9 supporting files (4091 lines) out of `package main` and into here.
// That was the plan, and measuring it changed the answer:
//
//   - Several platform helpers the move needs (shutdownSignals, sendGracefulStop) are ALSO used by
//     serve.go and main.go, which must stay. They would have to be duplicated or re-homed — and a
//     duplicated signal handler is precisely the class of divergence this package exists to remove.
//   - The move buys no safety over what is already true: `orchicon install` / `uninstall` ARE the single
//     implementation. A GUI that runs the CLI inherits every guard, every repair and every ordering fix
//     (including the port-resolution ordering that took a week to find) with no chance of drift, whereas
//     a copy of that logic in a library has to be kept in step for ever.
//   - Install and uninstall are minutes-long and REPORT THROUGH A FILE, not through a return value (see
//     SetupProgress). A library call would have to become asynchronous anyway to serve a GUI, so the
//     synchronous-function shape was never available for the verbs that mattered.
//
// SO THE SINGLE IMPLEMENTATION IS THE BINARY, and this package is the thin, typed, testable client of it.
// That is the same deliberate choice `host-services` already records: a re-exec of our own binary, on
// purpose, because the alternative is a second implementation.
//
// WHAT IS IN-PROCESS AND WHAT IS SPAWNED:
//
//	Status, List            read the instance's own state files (ports.json, pid files) — no process
//	Start, Stop, Restart    in-process via internal/planebind, so a GUI gets the detailed outcome
//	                        (already-running / left-alone-unidentified / failed) rather than a parsed log
//	Install, Uninstall      SPAWNED: `orchicon install|uninstall …`, detached, progress via file
//
// The split is by NATURE, not by convenience: the three short verbs have a rich in-process result worth
// keeping, and the two long ones cannot have one.
//
// IMPORT DISCIPLINE (asserted by imports_test.go): stdlib + internal/planebind + internal/planecontrol +
// internal/instance. Nothing that could pull the plane's HTTP server in, so this compiles inside the
// desktop shell and inside cmd/orch.
package instancectl

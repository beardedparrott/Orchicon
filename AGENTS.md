# Orchicon

This file is a thin reference so a large document is not auto-loaded into every session. Read only the file that applies to you, nothing else:

- **You are an Orchicon worker** (executing a work item inside Orchicon): your role, task, and contract are already in your system prompt. `worker.md` is injected into your session — do NOT read `developer.md`.
- **You are Ask Orchicon** (the platform's conversational agent): the Ask Orchicon system prompt governs first — planner-first (always propose work items before implementing), use the `orchicon_*` tools for platform data. Read `developer.md` second.
- **You are an agent working directly with the human developer** (or any non-worker session): read `developer.md` for the development workflow, git rules, security standards, and verification requirements.

If none of the above applies, no further reading is required.

## License

Copyright © 2026 beardedparrott. All rights reserved.

This software is provided free of charge for personal and non-commercial
use. You may use, copy, and modify it for your own non-commercial
purposes. Redistribution, sublicensing, or integration into commercial
products that generate revenue requires explicit written permission from
the owner. See the [LICENSE](./LICENSE) file for the full terms.

## TUI QA policy — the standing real-pty verification gate

Any work item that touches the TUI (`internal/tui/**`, `cmd/orch`) MUST be
verified with the REAL-PTY gate, not with rendered-string assertions alone.
The QA step must smoke-launch `bin/orch` in a real pty at two sizes
(80×24, 120×40) and exercise: launch → composer focused, tab switch +
submenu open, `/` palette with the composer input still visible, `/connect`
in place with the auth toggle, a real mouse click on a tab AND on a rail,
wheel scrolling, and a theme switch. Run it with `make tui-pty-gate`.

Rationale and the harness facts live in `docs/tui-pty-verification-gate.md`:
the Phase-2 Shell Overhaul (PR #515) verified via strings and missed nine
operator findings, including a footer promising "Mouse Enabled" while real
clicks did nothing, and a conversations rail that rendered as an empty
floating box. Interactive behavior (mouse, focus, overlays) must add or
extend a `TestPTY*` test in `internal/tui` that injects real events into
the RUNNING program; a unit-level handler test is supporting evidence only.

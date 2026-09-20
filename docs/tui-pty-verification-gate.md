# TUI Standing Real-PTY Verification Gate (Phase 2c, QA policy)

**Status:** mandatory QA policy for ANY TUI work item (shell, rails, mouse,
keys, palette, theming, /connect).

**Why this exists.** The Phase-2 feature (Shell Overhaul, PR #515) was
verified with rendered-STRING assertions only — "the view contains the
expected text". Real operators then found **nine** regressions the strings
could not see: the footer promised `Mouse Enabled` while clicks did nothing
(finding 7), and the conversations rail rendered as a floating "none — start
a chat" box with a stray top-left `error: unauthenticated` (finding 9).
**Rendered-string assertions alone are INSUFFICIENT.** A TUI claim is only
verified when a REAL `bin/orch` process runs on a REAL pty and its painted
byte stream proves the behavior.

The gate is implemented as Go tests in `internal/tui` (same single-reader
pty harness, `ORCH_PTY_SMOKE=1` opt-in for humans; CI runs it
non-interactively):

| Step | Test | What it proves |
|---|---|---|
| launch → composer focused | `TestPTYSmokeLaunchFullTakeoverComposerFocused` (80×24 + 120×40) | alt-screen takeover, exact-viewport paint, `❯` prompt, typed chars echo without ctrl+g |
| tab switch + submenu open | `TestPTYMouseGate` (5) | a real **mouse click** on the tab bar paints that tab's dropdown submenu |
| `/` palette with visible composer input | `TestPaletteAboveComposer` (unit) + `TestPTYMouseGate` (composer focus) | the palette floats ABOVE the composer; typed text stays visible |
| `/connect` in place with auth toggle | `TestPTYConnectOverlayInPlaceReconnect` | the overlay opens IN PLACE (PID unchanged), ctrl+a flips API key ↔ username+password, submit reconnects in place, esc cancels |
| real mouse click on a tab AND a rail | `TestPTYMouseGate` (2, 5, 6) | rail header collapses on click, a rail ROW click opens that conversation's transcript, wheel scrolls the rail |
| wheel scrolls lists/diffs/rails | `TestPTYMouseGate` (4) + `internal/tui/diffs` click/wheel tests | the rail's visible range moves |
| theme switch | `TestPTYMouseGate` (7) | `/theme dark` takes effect in the running shell |

## How to run the gate

```bash
make tui-pty-gate            # the standing gate (smoke + mouse + /connect)
ORCH_PTY_SMOKE=1 go test ./internal/tui/ -run 'TestPTY' -count=1 -v
```

CI (no controlling terminal) runs it automatically as part of
`go test ./...`; a human at an interactive terminal must opt in with
`ORCH_PTY_SMOKE=1` (a real-pty spawn of the live TUI hijacks the operator's
own terminal otherwise).

## Acceptance-criteria pattern for TUI work items

Every TUI work item's acceptance criteria MUST carry this line:

> **Real-pty verification gate:** the QA step must smoke-launch `bin/orch`
> in a real pty at two sizes (80×24, 120×40) and exercise: launch → composer
> focused, tab switch + submenu open, `/` palette with visible composer
> input, `/connect` in place with the auth toggle, a real mouse click on a
> tab and on a rail, theme switch. Rendered-string assertions alone are
> insufficient.

New interactive behavior (mouse handlers, focus rules, overlays) MUST add
or extend a `TestPTY*` test in `internal/tui` that injects the real events
(keys, SGR mouse sequences) into the RUNNING program and asserts the
painted output — a unit-level handler test is a supporting test, never the
verification.

## Known harness facts (do not re-derive)

- The pty harness keeps exactly ONE reader goroutine per session; phases
  snapshot the accumulated buffer (`readFor`). A new reader per phase
  races and starves reads.
- The harness must answer termenv's startup queries (OSC 11 bg color,
  DSR 6n cursor position) or the TUI blocks before its first paint.
- Mouse events use SGR encoding: `ESC [ < B ; X ; Y M` (press) / `m`
  (release); wheel up/down are buttons 64/65; X/Y are 1-based.
- Buffer-wide assertions are unreliable (the log accumulates every frame).
  Positive ("never painted before") markers can be asserted anywhere in
  the capture; NEGATIVE assertions ("text is gone") must look at a tight
  tail window, because bubbletea repaints only changed lines.

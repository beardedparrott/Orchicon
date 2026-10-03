package tui

// trace.go — TEMPORARY, OPT-IN DIAGNOSTIC. Off unless ORCHICON_TUI_TRACE names a file.
//
// WHY THIS EXISTS. A reported symptom — "my user messages get swallowed up until a turn starts" and "I
// still don't see the 'Orchicon is thinking...'" — could not be reproduced in-process: driving the REAL
// Enter key through dispatch() in a test produces the echo, the painted frame and the status line
// correctly, and every guard in the path evaluates the way it should. When a symptom will not reproduce,
// the honest next step is to STOP INFERRING and read the state from a real session, which is what this
// file is for.
//
// It is a TRACE, not a logger: one line per decision point on the send → paint path, appended to the file
// the operator names. Nothing here changes behaviour — every call is a no-op when the variable is unset,
// and a trace that cannot be written is silently dropped (a diagnostic must never break the TUI).
//
// IT IS DELETABLE. Once the state is read and the cause is fixed, this file and its call sites come out.
//
//	ORCHICON_TUI_TRACE=/tmp/orch-trace.log ./orch
//
// The points it records are the ones that split the possible causes apart:
//
//	composerKey      did the DOCK hand a send to the shell? (its absence means the key never got there)
//	sendFromComposer did the ECHO get appended, and to which conversation? (its absence means a
//	                 swallowed send: the dock parked the text and nobody collected it)
//	onChatWake       was the pane repainted at all, and if not, WHICH guard stopped it?
//	paint            what was painted, with the guard state that decided it

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// traceFile is the operator-named destination, resolved once. Empty = tracing off.
var traceFile = os.Getenv("ORCHICON_TUI_TRACE")

var traceMu sync.Mutex

// tracef appends one line to the trace file. No-op when tracing is off.
func tracef(format string, args ...any) {
	if traceFile == "" {
		return
	}
	// A diagnostic must never take the shell down with it, so every failure here is swallowed.
	f, err := os.OpenFile(traceFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	traceMu.Lock()
	defer traceMu.Unlock()
	_, _ = fmt.Fprintf(f, "%s %s\n", time.Now().Format("15:04:05.000"), fmt.Sprintf(format, args...))
}

// Package acp is a Go client for the Antigravity CLI's ACP (Agent Client Protocol) mode,
// `agy acp`.
//
// It speaks newline-delimited JSON-RPC 2.0 over the child's stdin/stdout: it spawns the
// process, performs the `initialize`/`authenticate` handshake, opens and drives sessions
// (`session/new`, `session/load`, `session/resume`, `session/set_model`, `session/set_mode`,
// `session/prompt`, `session/cancel`, `session/close`), decodes every `session/update`
// notification into a Go sum type, and answers the agent's client-side requests
// (`session/request_permission`) through a Handler.
//
// The wire contract implemented here is the one recorded from agy 1.5.13 and documented in
// docs/adapters/antigravity-acp.md; the fixtures under internal/antigravity/testdata/acp are
// replayed by this package's tests.
//
// There is NO platform wiring in this package. It is a library the antigravity worker bridge
// and the antigravity Ask bridge share, so there is exactly one description of an ACP turn
// rather than two that can drift.
//
// Two rules are load-bearing and easy to break:
//
//   - The reader goroutine must NEVER block on a consumer. Agent requests are served on their
//     own goroutines and session updates are queued, so a Handler that blocks on an operator
//     decision does not stall the rest of the messages (a permission prompt and a streamed
//     answer arrive on the same wire).
//   - The client fails CLOSED. Any Handler error, panic, cancellation or teardown answers a
//     permission request with the reject option, never an allow.
package acp

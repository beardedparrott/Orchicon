package acp

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/beardedparrott/orchicon/internal/adapter"
)

// JSON-RPC / ACP error codes. The first four are JSON-RPC 2.0; the last is LOCAL to this client
// (it is never sent on the wire) and is used to fail pending calls when the child goes away.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603

	// CodeClientClosed is synthesised by THIS client when the process/session is gone.
	CodeClientClosed = -32099
)

// RPCError is a JSON-RPC error object returned by agy.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return "acp: <nil rpc error>"
	}
	if len(e.Data) > 0 {
		return fmt.Sprintf("acp: rpc error %d: %s (%s)", e.Code, e.Message, string(e.Data))
	}
	return fmt.Sprintf("acp: rpc error %d: %s", e.Code, e.Message)
}

// ErrClosed reports a use of a client whose child process is gone. ClosedError carries the
// details and matches this sentinel via errors.Is.
var ErrClosed = errors.New("acp: client closed")

// ClosedError is returned by every call that cannot complete because the agy process exited
// (or the client was closed). It carries the exit code and the tail of stderr, because a
// transport failure without the child's own diagnostic is unactionable.
type ClosedError struct {
	ExitCode int
	Cause    error
	Stderr   []string
}

func (e *ClosedError) Error() string {
	var b strings.Builder
	b.WriteString("acp: client closed")
	if e.ExitCode != 0 {
		fmt.Fprintf(&b, " (exit code %d)", e.ExitCode)
	}
	if e.Cause != nil && !errors.Is(e.Cause, ErrClosed) {
		fmt.Fprintf(&b, ": %v", e.Cause)
	}
	if len(e.Stderr) > 0 {
		b.WriteString("; stderr: ")
		b.WriteString(strings.Join(e.Stderr, " | "))
	}
	return b.String()
}

// Is makes errors.Is(err, ErrClosed) true for every closed-connection failure.
func (e *ClosedError) Is(target error) bool { return target == ErrClosed }

// Unwrap exposes ErrClosed so errors.Is keeps working through wrapping.
func (e *ClosedError) Unwrap() error { return ErrClosed }

// AuthError is the ONE actionable authentication failure for the antigravity adapter kind.
//
// EVERY error from `authenticate` is mapped here, not just the ones the shared signal list
// recognises: agy reports a missing API key as -32602 "GEMINI_API_KEY is not set; pass an API key
// in _meta.apiKey or set the GEMINI_API_KEY environment variable", which matches none of
// adapter.AntigravityAuthFailure's signals, and a mapper that let it through would turn the one
// actionable failure into a generic RPC error.
type AuthError struct {
	// Message is the operator-facing instruction — adapter.AntigravityAuthRequiredMessage.
	Message string
	// Detail is agy's own text, kept for the log/trace.
	Detail string
	// Cause is the underlying RPC error.
	Cause error
}

func (e *AuthError) Error() string {
	if e.Detail != "" && e.Detail != e.Message {
		return e.Message + " (agy: " + e.Detail + ")"
	}
	return e.Message
}

func (e *AuthError) Unwrap() error { return e.Cause }

// authErrorFrom maps ANY authenticate failure onto an *AuthError.
func authErrorFrom(err error) *AuthError {
	if err == nil {
		return nil
	}
	var rpcErr *RPCError
	detail := err.Error()
	if errors.As(err, &rpcErr) {
		detail = rpcErr.Message
	}
	msg := adapter.AntigravityAuthRequiredMessage
	if mapped, ok := adapter.AntigravityAuthFailure(detail); ok {
		msg = mapped
	}
	return &AuthError{Message: msg, Detail: detail, Cause: err}
}

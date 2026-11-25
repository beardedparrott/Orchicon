package acp

// handler.go — THE CLIENT-SIDE REQUESTS agy makes, and the fail-closed answer.
//
// agy 1.5.13 sends exactly ONE client-side request: session/request_permission (docs 2.3). It
// never issues fs/* or terminal/*, even though the recorder advertised those capabilities.
//
// THE HANDLER MAY BLOCK. The operator decision it waits on can take minutes, and the turn is
// still streaming while it waits, so the conn serves requests on their own goroutines and never
// lets a handler hold the reader.
//
// THE CLIENT FAILS CLOSED. A handler error, panic, cancellation, teardown, a nil handler or an
// answer naming an option agy did not offer all resolve to the REJECT option. There is no path
// here that answers a permission request with an allow the operator did not give.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// Handler answers the agent's client-side requests.
type Handler interface {
	// RequestPermission asks for a decision on a tool call. Returning an error is safe: the
	// client answers with the reject option.
	RequestPermission(ctx context.Context, req PermissionRequest) (PermissionDecision, error)
}

// PermissionRequest is one asked-for tool call.
type PermissionRequest struct {
	SessionID string
	ToolCall  ToolCallInfo
	Options   []PermissionOption
	// Raw is the request params exactly as agy sent them.
	Raw json.RawMessage
}

// PermissionDecision is the handler's answer. A zero decision (neither OptionID nor Cancelled)
// is treated as a handler error and fails closed.
type PermissionDecision struct {
	// OptionID must be one of the optionIds in the request.
	OptionID string
	// Cancelled answers `{"outcome":{"outcome":"cancelled"}}` — used when the turn is gone
	// (session cancel/close), never as a way to say "allow".
	Cancelled bool
}

// RejectOptionID picks the reject answer from an offered set: first a reject KIND, then a
// deny/reject/no-looking id or name, and false when agy offered no reject at all. Exported
// because both bridges need to reason about the same list, and because the fail-closed path in
// conn.go uses exactly this.
func RejectOptionID(opts []PermissionOption) (string, bool) {
	for _, o := range opts {
		if o.Kind == OptionKindRejectOnce || o.Kind == OptionKindRejectAlways {
			return o.OptionID, true
		}
	}
	for _, o := range opts {
		if looksLike(o, "deny") || looksLike(o, "reject") || looksLike(o, "no") {
			return o.OptionID, true
		}
	}
	return "", false
}

// AllowOptionID picks the allow answer, reporting false when agy offered one. It exists so a
// caller (and the tests) can assert an allow was really offered rather than assumed.
func AllowOptionID(opts []PermissionOption) (string, bool) {
	for _, o := range opts {
		if o.Kind == OptionKindAllowOnce || o.Kind == OptionKindAllowAlways {
			return o.OptionID, true
		}
	}
	for _, o := range opts {
		if looksLike(o, "allow") || looksLike(o, "approve") || looksLike(o, "yes") {
			return o.OptionID, true
		}
	}
	return "", false
}

// looksLike reports whether an option's id or name starts with want (case-insensitive).
func looksLike(o PermissionOption, want string) bool {
	return strings.HasPrefix(strings.ToLower(o.OptionID), want) ||
		strings.HasPrefix(strings.ToLower(o.Name), want)
}

// isOffered reports whether optionID is one of the offered options.
func isOffered(opts []PermissionOption, optionID string) bool {
	for _, o := range opts {
		if o.OptionID == optionID {
			return true
		}
	}
	return false
}

// ErrNoHandler reports that no permission Handler was configured. It fails closed.
var ErrNoHandler = errors.New("acp: no permission handler configured")

// rejectEverything is the handler used when Config.Handler is nil. It never allows anything.
type rejectEverything struct{}

func (rejectEverything) RequestPermission(context.Context, PermissionRequest) (PermissionDecision, error) {
	return PermissionDecision{}, ErrNoHandler
}

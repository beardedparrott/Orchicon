package claude

import "encoding/json"

// control.go — the verdict half of claude's control protocol.
//
// A session running with an interactive permission profile does not decide a
// tool call itself: the CLI raises a `can_use_tool` control_request on stdout
// and WAITS for a control_response on stdin. This file builds that response.
// Decoding the request is in stream.go (StreamEvent.ControlSubtype ==
// "can_use_tool").
//
// THE WIRE SHAPE IS NOT GUESSED. It was read out of the installed binary
// (2.1.261), which carries both its own serializer and the schema it validates
// against:
//
//	return {type:"control_response", response:{
//	  subtype:"success", request_id:e, response:r }}
//
//	Expected {behavior: 'allow', updatedInput?: object} or
//	         {behavior: 'deny', message: string}.
//
// Two details are load-bearing and easy to get wrong by inference:
//
//  1. request_id is nested INSIDE `response`, not a sibling of it. A frame with
//     a top-level request_id is silently unmatched, and the CLI's tool call
//     stays parked until the turn is cancelled — an Ask turn that hangs with a
//     consent card on screen the operator already answered.
//  2. The verdict is a THIRD level down, at response.response — so the same
//     field name appears twice on the way to it. The CLI reads it as
//     `f.response.response?.behavior`.
//
// A third `behavior` value exists for settling rather than deciding: the binary
// emits {behavior:"cancelled"} when it cancels a dialog by machine (an
// interrupted turn, or an ask another client already answered), which is the
// CLI -> host direction. This encoder sends decisions only, plus the explicit
// cancel the host sends when it retires a card it can no longer answer.

// The `behavior` discriminator on a permission verdict.
const (
	// BehaviorAllow proceeds with the call. When UpdatedInput is set the CLI
	// runs the MODIFIED input instead of what the model proposed.
	BehaviorAllow = "allow"
	// BehaviorDeny refuses the call. Message is REQUIRED: the CLI surfaces it
	// to the model as the tool result, so a deny the model cannot act on is a
	// worse outcome than the refusal itself.
	BehaviorDeny = "deny"
	// BehaviorCancelled retires an ask without answering it — used when the
	// turn is superseded or the conversation is deleted and the card on screen
	// is no longer answerable.
	BehaviorCancelled = "cancelled"
)

// decisionClassification values. The binary documents these as matching the
// tool_decision OTel vocabulary, so the consent decision is attributable
// downstream instead of collapsing into a bare allow/deny:
//
//	"user_temporary for allow-once, user_permanent for always-allow (both the
//	 click and later cache hits), user_reject for deny. If unset, the CLI infers
//	 conservatively (temporary for allow, reject for deny)."
const (
	// DecisionUserTemporary is a one-shot approval ("allow once").
	DecisionUserTemporary = "user_temporary"
	// DecisionUserPermanent is a standing approval ("always allow"), i.e. a
	// session grant that later cache hits reuse.
	DecisionUserPermanent = "user_permanent"
	// DecisionUserReject is a refusal by the operator.
	DecisionUserReject = "user_reject"
)

// ControlVerdict is the object the CLI validates. Field presence follows the
// CLI's own union: `message` is meaningful only for a deny (it is what the
// model reads), and `updatedInput` only for an allow.
type ControlVerdict struct {
	Behavior string `json:"behavior"`
	// Message is the human/model-readable reason. REQUIRED on deny; omitted on
	// allow so the frame matches the schema's allow arm exactly.
	Message string `json:"message,omitempty"`
	// UpdatedInput replaces the model's proposed input for an allowed call.
	// Unused today (the ask transport approves or refuses as asked) and carried
	// so the encoder is complete rather than silently dropping a field the
	// protocol defines.
	UpdatedInput map[string]any `json:"updatedInput,omitempty"`
	// DecisionClassification records the KIND of decision (see the constants).
	DecisionClassification string `json:"decisionClassification,omitempty"`
}

// controlResponseDoc is the outer frame. It is a struct rather than a map so the
// nesting that the CLI matches on cannot be flattened by a later refactor into
// what looks like tidier JSON.
type controlResponseDoc struct {
	Type     string               `json:"type"`
	Response controlResponseInner `json:"response"`
}

type controlResponseInner struct {
	Subtype   string         `json:"subtype"`
	RequestID string         `json:"request_id"`
	Response  ControlVerdict `json:"response"`
}

// EncodePermissionResponse builds the FULL control_response frame for one ask.
// requestID is the `request_id` of the can_use_tool control_request being
// answered (StreamEvent.ControlRequestID) — echoing it is what correlates the
// answer to the parked tool call.
func EncodePermissionResponse(requestID string, verdict ControlVerdict) []byte {
	doc := controlResponseDoc{
		Type: "control_response",
		Response: controlResponseInner{
			Subtype:   "success",
			RequestID: requestID,
			Response:  verdict,
		},
	}
	out, err := json.Marshal(doc)
	if err != nil {
		// Cannot happen for this shape; a nil return makes the caller's write a
		// no-op rather than sending a malformed frame the CLI would drop.
		return nil
	}
	return out
}

// AllowResponse builds an approval frame. classification is one of the
// Decision* constants ("" lets the CLI infer).
func AllowResponse(requestID, classification string) []byte {
	return EncodePermissionResponse(requestID, ControlVerdict{
		Behavior:               BehaviorAllow,
		DecisionClassification: classification,
	})
}

// DenyResponse builds a refusal frame. message is required by the CLI and is
// what the model is told, so it must say something actionable.
func DenyResponse(requestID, message, classification string) []byte {
	if message == "" {
		message = "refused by the operator"
	}
	return EncodePermissionResponse(requestID, ControlVerdict{
		Behavior:               BehaviorDeny,
		Message:                message,
		DecisionClassification: classification,
	})
}

// CancelResponse retires an ask that can no longer be answered, so the CLI stops
// waiting on it (a superseded turn, a deleted conversation, a settled card).
func CancelResponse(requestID string) []byte {
	return EncodePermissionResponse(requestID, ControlVerdict{Behavior: BehaviorCancelled})
}

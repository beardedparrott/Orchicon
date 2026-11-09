package orchicon

// requestrepair.go — REPAIRING A REQUEST THE PROVIDER REJECTED, AND RESENDING IT.
//
// THE OPERATOR: "if we run into an error about prompts/requests then we should reshape them automatically to
// fix the underlying issue and resend." That is the other half of resilience, and it is the half a retry
// cannot cover: a transient failure (429, 502, a reset connection) is worth RETRYING unchanged, but a
// REQUEST-SHAPE failure is worth REPAIRING, because resending the same bytes would fail identically.
//
// The distinction is the whole design:
//
//	retryable  → the provider or the network was briefly unhappy. Same request, later. (doWithRetries)
//	repairable → the request itself is wrong in a way this layer can see and fix. DIFFERENT request, now.
//	fatal      → neither. Surface it.
//
// WHAT MAKES THIS WORTH HAVING rather than a provider quirk table: every case here is a shape the provider
// TOLD us about, in its own error text. The repair is driven by what the response says, not by a guess about
// which model is on the other end — so it works for a provider nobody has seen yet, and it cannot silently
// mangle a request that was fine.
//
// THE CASE THAT PROMPTED IT (and it is a real one, measured):
//
//	400 invalid_request_error: The `reasoning_content` in the thinking mode must be passed back to the API.
//
// DeepSeek's thinking mode requires an assistant message's reasoning back on the next request. The adapter had
// always CAPTURED reasoning for display but had nowhere to store it on a Message, so every second round of a
// tool-using turn was rejected. It presented as intermittent provider flakiness because it only bit turns that
// made a tool call and then continued.
//
// The durable fix for that one is upstream (Content.Reasoning, Quirks.ReplayReasoning), and it is done — this
// layer is the BELT to that braces: it catches the class, including future members of it, and it also repairs
// a request that arrives already malformed from somewhere else.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// RepairKind names a recognised, fixable request-shape problem.
type RepairKind string

const (
	// RepairNone means the error is not a recognised repairable shape.
	RepairNone RepairKind = ""

	// RepairReasoningContent is a provider demanding an assistant message's reasoning back (DeepSeek thinking
	// mode). The repair replays the stored reasoning.
	RepairReasoningContent RepairKind = "reasoning_content"

	// RepairUnknownField is a provider rejecting a field this request sent. The repair drops it.
	RepairUnknownField RepairKind = "unknown_field"

	// RepairToolMessageOrder is a provider rejecting the message sequence (a tool result with no preceding
	// tool call, or an assistant tool call with no result). The repair drops the orphaned message.
	RepairToolMessageOrder RepairKind = "tool_message_order"
)

// classifyRepairable inspects a provider error and reports whether this layer can repair the
// request. IT REQUIRES POSITIVE EVIDENCE — a status of 400 AND a phrase the provider itself used.
func classifyRepairable(statusCode int, body string) RepairKind {
	if statusCode != http.StatusBadRequest {
		return RepairNone
	}
	b := strings.ToLower(body)
	switch {
	case strings.Contains(b, "reasoning_content") &&
		(strings.Contains(b, "passed back") || strings.Contains(b, "must be") || strings.Contains(b, "required")):
		return RepairReasoningContent
	case strings.Contains(b, "unknown field") || strings.Contains(b, "unrecognized field") ||
		strings.Contains(b, "additional propert") || strings.Contains(b, "extra fields not permitted"):
		return RepairUnknownField
	case strings.Contains(b, "tool_call_id") ||
		(strings.Contains(b, "tool") && strings.Contains(b, "message") &&
			(stringstrings(b, "must be", "sequence", "precede", "follow", "without"))):
		return RepairToolMessageOrder
	}
	return RepairNone
}

// stringstrings reports whether ANY of the needles appears in s. It exists so a multi-phrase case reads as one
// condition rather than a chain of ||.
func stringstrings(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// repairRequest applies a recognised repair to a request, and reports whether it CHANGED anything.
func repairRequest(kind RepairKind, req *TurnRequest) (changed bool) {
	switch kind {
	case RepairReasoningContent:
		// The reasoning is already on the messages when the bridge captured it; this repair covers the case
		// where the provider demands it and the request would otherwise go out WITHOUT it.
		for i := range req.Messages {
			for j := range req.Messages[i].Content {
				if r := req.Messages[i].Content[j].Reasoning; r != nil && *r != "" {
					return true
				}
			}
		}
		return false

	case RepairToolMessageOrder:
		// Drop assistant tool calls that have no matching tool result, and tool results with no matching
		// call.
		calls := map[string]bool{}
		results := map[string]bool{}
		for _, m := range req.Messages {
			for _, c := range m.Content {
				if c.ToolUse != nil && c.ToolUse.ToolCallID != "" {
					calls[c.ToolUse.ToolCallID] = true
				}
				if c.ToolResult != nil && c.ToolResult.ToolCallID != "" {
					results[c.ToolResult.ToolCallID] = true
				}
			}
		}
		out := make([]Message, 0, len(req.Messages))
		for _, m := range req.Messages {
			kept := make([]Content, 0, len(m.Content))
			for _, c := range m.Content {
				switch {
				case c.ToolUse != nil && c.ToolUse.ToolCallID != "" && !results[c.ToolUse.ToolCallID]:
					continue // an assistant tool call with no result
				case c.ToolResult != nil && c.ToolResult.ToolCallID != "" && !calls[c.ToolResult.ToolCallID]:
					continue // a tool result with no call
				default:
					kept = append(kept, c)
				}
			}
			// A message left with nothing to say is dropped rather than sent empty.
			if len(kept) == 0 && m.Role != RoleAssistant {
				continue
			}
			if len(kept) == 0 {
				continue
			}
			m.Content = kept
			out = append(out, m)
		}
		if len(out) == len(req.Messages) {
			// Same count is not proof of no change — but the CONTENT could still have shrunk, so compare
			// total blocks, which is what the provider sees.
			before, after := 0, 0
			for _, m := range req.Messages {
				before += len(m.Content)
			}
			for _, m := range out {
				after += len(m.Content)
			}
			if before == after {
				return false
			}
		}
		req.Messages = out
		return true

	case RepairUnknownField:
		// Deliberately NOT repaired here.
		return false
	}
	return false
}

// learnedReplayReasoning records the providers that have DEMANDED an assistant message's reasoning
// back, keyed by provider id.
var learnedReplayReasoning sync.Map // provider key → struct{}

// ReplayReasoningLearned reports whether this provider has ever demanded reasoning replay.
func ReplayReasoningLearned(providerKey string) bool {
	_, ok := learnedReplayReasoning.Load(providerKey)
	return ok
}

// noteReplayReasoningDemanded records the demand, reporting true the FIRST time so the caller logs it once
// rather than once per request.
func noteReplayReasoningDemanded(providerKey string) (first bool) {
	_, loaded := learnedReplayReasoning.LoadOrStore(providerKey, struct{}{})
	return !loaded
}

// forgetLearnedReplayReasoning clears the learned set (tests; a provider's profile can legitimately change
// under a live process via the settings UI).
func forgetLearnedReplayReasoning() { learnedReplayReasoning = sync.Map{} }

// reasoningReplayReport describes what the LAST request actually carried, for the error a caller
// surfaces.
func reasoningReplayReport(req TurnRequest, replayOn bool, repairs int) string {
	assistant, withReasoning, callsWithout := 0, 0, 0
	for _, m := range req.Messages {
		if m.Role != RoleAssistant {
			continue
		}
		assistant++
		has, calls := false, false
		for _, c := range m.Content {
			if c.Reasoning != nil && *c.Reasoning != "" {
				has = true
			}
			if c.ToolUse != nil {
				calls = true
			}
		}
		if has {
			withReasoning++
		} else if calls {
			callsWithout++
		}
	}
	onoff := "off"
	if replayOn {
		onoff = "on"
	}
	return fmt.Sprintf("reasoning replay %s: %d of %d assistant messages carried reasoning_content, "+
		"%d tool-calling message(s) had none to return, %d repair resend(s)",
		onoff, withReasoning, assistant, callsWithout, repairs)
}

// statusErrRepairable extracts the pieces classifyRepairable needs from an error.
func statusErrRepairable(err error) (RepairKind, bool) {
	var se *StatusError
	if !errors.As(err, &se) {
		return RepairNone, false
	}
	return classifyRepairable(se.StatusCode, se.Body), true
}

// sendWithRepair is the SHARED send loop: retry transient failures, repair request-shape failures
// and resend, surface anything else.
func sendWithRepair(
	ctx context.Context,
	p RetryPolicy,
	logf func(string, ...any),
	post func(ctx context.Context, body []byte) (*http.Response, error),
	marshal func() ([]byte, error),
	req *TurnRequest,
	onRepair func(RepairKind) (changed bool),
) (*http.Response, error) {
	for repair := 0; ; repair++ {
		body, merr := marshal()
		if merr != nil {
			return nil, merr
		}
		var resp *http.Response
		err := doWithRetries(ctx, p, func(attempt int) (bool, error, time.Duration) {
			r, err2 := post(ctx, body)
			if err2 != nil {
				return isConnectionErr(err2), err2, 0
			}
			if r.StatusCode >= 200 && r.StatusCode < 300 {
				resp = r
				return false, nil, 0
			}
			b, _ := io.ReadAll(io.LimitReader(r.Body, 8192))
			_ = r.Body.Close()
			e := httpStatusError(r.StatusCode, r.Status, b)
			if retryableStatus(r.StatusCode) {
				ra, _ := RetryAfter(r.Header.Get("Retry-After"), time.Now())
				return true, e, ra
			}
			return false, e, 0
		})
		if err == nil {
			return resp, nil
		}
		kind, _ := statusErrRepairable(err)
		if kind == RepairNone || repair >= repairAttempts {
			return nil, err
		}
		if onRepair == nil || !onRepair(kind) {
			// Nothing this layer can change: surface the provider's own error rather than loop on it.
			return nil, err
		}
		if logf != nil {
			logf("repaired request and resending (%s) after: %v", kind, err)
		}
	}
}

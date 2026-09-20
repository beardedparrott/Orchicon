package askorchicon

// modegate.go — the NATIVE adapter's application of the mode boundary.
//
// The POLICY itself is not here any more: it lives in internal/askmode, because it is a property of the modes
// rather than of this adapter, and the operator is adding more adapters ("we should definitely build the agnostic
// approach now for all adapters instead of waiting"). This file is the native adapter's two responsibilities:
// carrying the turn's mode on its context, and turning a denial into a MESSAGE the model can relay.
//
// The operator's requirement, which this is the enforcement of: "Each mode of Ask Orchicon must NEVER just do the
// work it is not supposed to do and must always enforce the user switch the mode first no matter what the user
// says." "No matter what the user says" is not something a prompt can deliver — a user can argue any model past
// its instructions, which is what a prompt is — so the refusal is at the layer that EXECUTES the call.

import (
	"context"
	"fmt"

	"github.com/beardedparrott/orchicon/internal/askmode"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// withAskMode stamps a turn's context with the mode it is running under.
//
// A thin wrapper over askmode so this package's call sites (and tests) keep one name for it, while the KEY and the
// lookup live somewhere every adapter can use.
func withAskMode(ctx context.Context, mode string) context.Context {
	return askmode.WithMode(ctx, mode)
}

// askModeFromContext reads the mode stamped on a turn. "" when there is none.
func askModeFromContext(ctx context.Context) string {
	return askmode.ModeFromContext(ctx)
}

// --- the turn's conversation ----------------------------------------------------------------

// ctxKeyConversation is the unexported context key for the turn's conversation id.
//
// WHY IT IS ON THE CONTEXT rather than passed as a parameter. A tool that reports facts about THIS session —
// which conversation it is in, which mode it runs under, which model_ref it resolves to — cannot ask the caller
// for them: the model would be guessing at its own identity, and "which model am I on" is exactly the fact a
// Quick Work dispatch has to state out loud before it pins one into a worker. The id is stamped in the SAME
// place the mode is (chat.go's startConversationTurnOpts) and rides the same path to the tool boundary, so both
// halves of a turn's self-knowledge come from one read of one row.
type ctxKeyConversation struct{}

// withAskConversation stamps a turn's context with its conversation id.
func withAskConversation(ctx context.Context, convID string) context.Context {
	return context.WithValue(ctx, ctxKeyConversation{}, convID)
}

// askConversationFromContext reads the conversation id stamped on a turn. "" when there is none — a test driving
// a tool directly, or a caller that never stamped one. A tool that REQUIRES the id must fail loud on "" rather
// than substituting a guess, because a confidently wrong answer about this session is worse than an error.
func askConversationFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(ctxKeyConversation{}).(string)
	return v
}

// applyAskToolPolicy hands the turn's policy to the adapter, and — the important half — reports when the adapter
// cannot enforce it.
//
// THE HONEST REPORT IS THE POINT. Whether an adapter can restrict tools per turn depends on whether its mechanism
// is per-invocation or per-process, and the platform cannot assume either. An adapter that does not implement
// scheduler.ChatToolRestrictor is running a turn where the mode boundary is PROSE ONLY — real, but advisory — and
// saying so in the log is what stops that from being discovered later, from a mode doing something it should not
// have, with nothing to point at.
//
// It is a log line rather than a refusal because an adapter without the capability is a KNOWN state, not an
// error: the boundary is still described in the prompt, the mode is still stamped on the turn, and a second
// adapter is a work item rather than an outage.
func (s *Service) applyAskToolPolicy(ctx context.Context, client scheduler.ChatTurnClient, mode, modelRef string) {
	denied := askmode.DeniedNames(mode)
	if len(denied) == 0 {
		return // no policy for this mode (or no mode at all): nothing to apply, nothing to report
	}
	if r, ok := client.(scheduler.ChatToolRestrictor); ok {
		if err := r.RestrictChatTools(ctx, scheduler.ToolPolicy{Mode: mode, Denied: denied}); err != nil && s.log != nil {
			s.log.Warn("ask: the adapter could not apply the mode's tool policy — the boundary is ADVISORY for this turn",
				"mode", mode, "model_ref", modelRef, "error", err)
		}
		return
	}
	if s.log != nil {
		s.log.Info("ask: this adapter cannot restrict tools — the mode boundary is PROSE ONLY on this turn",
			"mode", mode, "model_ref", modelRef, "should_be_denied", denied)
	}
}

// modeAllowsTool reports whether the mode may run tool, and when it may not, the refusal to hand back.
//
// THE DECISION IS askmode's; only the WORDING is here. It states the boundary, says the PLATFORM refuses it rather
// than the model choosing to, and names the one action that resolves it — the user switching the mode. That last
// part is the operator's requirement made literal ("must always enforce the user switch the mode first"), and it
// is true: there is no mode-setting tool for the model to call instead.
//
// A mode with no policy (empty, or a value written by an older build) allows everything — see askmode.Allows.
func modeAllowsTool(mode, tool string) (bool, string) {
	if askmode.Allows(mode, tool) {
		return true, ""
	}
	p, _ := askmode.PolicyFor(mode)
	return false, fmt.Sprintf(
		"REFUSED BY THE PLATFORM: %q is not available in %s mode, so it was NOT executed. %s\n\n"+
			"You cannot override this, and you cannot switch your own mode — only the user can, from the mode "+
			"selector on this conversation. Say plainly that this is a %s job, name the mode, and ASK THE USER "+
			"TO SWITCH TO %s. Do not attempt the call again, and do not work around it with another tool.",
		tool, modeLabel(mode), p.Why, modeLabel(p.SwitchTo), modeLabel(p.SwitchTo))
}

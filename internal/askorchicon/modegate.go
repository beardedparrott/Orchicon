package askorchicon

// The operator's requirement, which this is the enforcement of: "Each mode of Ask Orchicon must
// NEVER just do the work it is not supposed to do and must always enforce the user switch the mode.

import (
	"context"
	"fmt"

	"github.com/beardedparrott/orchicon/internal/askmode"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// withAskMode stamps a turn's context with the mode it is running under.
func withAskMode(ctx context.Context, mode string) context.Context {
	return askmode.WithMode(ctx, mode)
}

// askModeFromContext reads the mode stamped on a turn. "" when there is none.
func askModeFromContext(ctx context.Context) string {
	return askmode.ModeFromContext(ctx)
}

// --- the turn's conversation ----------------------------------------------------------------

// WHY IT IS ON THE CONTEXT rather than passed as a parameter.

// withAskConversation stamps a turn's context with its conversation id.
func withAskConversation(ctx context.Context, convID string) context.Context {
	s := askmode.ConversationScopeFromContext(ctx)
	s.ConversationID = convID
	return askmode.WithConversationScope(ctx, s)
}

// A tool that REQUIRES the id must fail loud on "" rather than substituting a guess, because a
// confidently wrong answer about this session is worse than an error.
func askConversationFromContext(ctx context.Context) string {
	return askmode.ConversationScopeFromContext(ctx).ConversationID
}

// --- the conversation's PROJECT -------------------------------------------------------------------

// ctxKeyConversationProject is the unexported context key for the PROJECT the turn's conversation
// belongs to.

// withAskConversationProject stamps a turn's context with its conversation's project id. An empty id clears it to
// the unassigned view, which is exactly what an unassigned conversation wants.
func withAskConversationProject(ctx context.Context, projectID string) context.Context {
	s := askmode.ConversationScopeFromContext(ctx)
	s.ProjectID = projectID
	return askmode.WithConversationScope(ctx, s)
}

// askConversationProjectFromContext reads the conversation's project id stamped on a turn.
func askConversationProjectFromContext(ctx context.Context) string {
	return askmode.ConversationScopeFromContext(ctx).ProjectID
}

// An adapter that does not implement scheduler.ChatToolRestrictor is running a turn where the mode
// boundary is PROSE ONLY — real, but advisory.
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

// modeAllowsTool reports whether the mode may run tool, and when it may not, the refusal to hand
// back.
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

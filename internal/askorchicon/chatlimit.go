package askorchicon

// chatlimit.go — REACTIVE context-limit recovery for Ask turns.
//
// THE GAP THIS CLOSES. Automatic Ask compaction only ever existed on the two
// paths that PREDICT the overflow:
//
//   - the proactive pressure gate (orchicon/askpressure.go), which compacts
//     before a turn when the last MEASURED prompt size crossed a fraction of the
//     model's LIVE context window;
//   - the native reduction ladder (orchicon/askreduce.go), which reduces the
//     replayed history when a provider rejects the stream for size.
//
// Both live inside the NATIVE bridge. A SESSION-FUL adapter (opencode) has
// neither: it implements scheduler.ChatCompactor, but the only caller was the
// manual CompactConversation RPC behind /compact. So a conversation that reached
// its window on the opencode wire did not compact AT ALL — the serve returned a
// session.error, the collector turned it into a terminal failure, and the
// operator saw:
//
//	turn failed: Your request resulted in more tokens than this model's maximum
//	context length allows. Please reduce the length of the messages or run
//	compaction.
//
// They were told to run a command the plane can run for itself. The pressure gate
// cannot save them either: it is disarmed whenever the model has no live window
// hint (contextwindow.go: "compaction must never guess a window"), and free-tier
// models — whose small windows are hit ROUTINELY rather than by a freak
// 1M-token session — are exactly the ones a provider's ListModels may not
// describe.
//
// WHY THE FAILURE IS THE RIGHT TRIGGER. The error IS the missing fact. The
// proactive gate has to ESTIMATE when a window will be crossed, and its own
// contract is that it must never guess. A context-limit error is the provider
// stating, authoritatively and with no estimation, that this conversation is over
// its window. Reactive recovery cannot be armed with a wrong threshold; it can
// only fail to fire.
//
// WHY THE ERROR IS THEN HIDDEN. The operator asked for a conversation that keeps
// going, not for a status report on the plane's internals. If the compaction
// succeeds and the re-dispatch produces an answer, the failure never happened as
// far as the conversation goes — and the transcript already carries the durable
// system-role NOTICE ("Context compacted…"), which is the one visible trace the
// collapse is supposed to leave. Reporting the recovered error as well would show
// a red bubble for a turn that worked, the same class of lie the transcript's
// other paths go out of their way to avoid. The log line keeps the error, so
// nothing is lost for diagnosis.
//
// WHEN THE ERROR IS STILL SHOWN, in full, with the original text:
//
//   - the adapter cannot compact at all (no ChatCompactor capability);
//   - the compaction failed, or reported nothing collapsed;
//   - the retry hit the limit again (ONE retry: a second compaction of an
//     already-compacted conversation would cost the same detail and prove
//     nothing), or failed for any other reason.
//
// The budget is deliberately one retry per turn rather than a loop. A loop
// against a hard limit is how a turn burns its whole reply window failing slowly.

import (
	"context"

	"github.com/beardedparrott/orchicon/internal/orchicon"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// contextLimitReason is the AskCompactNotice reason for a reduction performed
// because the model reported a context-limit overflow. It is part of that
// vocabulary's documented set, and it is what compactionNoticeContent's own
// "context_limit" paragraph describes — the seam this file finally produces.
const contextLimitReason = "context_limit"

// compactAfterContextLimit compacts a conversation whose turn just failed on a
// context-limit error, so the collector can re-dispatch the same message.
//
// ok is true only when a compaction really happened and a retry is therefore
// worth attempting. Every other outcome — no capability, a compaction error, a
// conversation with nothing left to collapse — returns false with a reason, and
// the caller surfaces the ORIGINAL failure, which names the real limit.
//
// sessionID is the session the failing turn was on. A session-ful adapter needs
// it (and the model) to summarize anything at all; a sessionless one derives its
// own id from the conversation id.
//
// The notice is recorded HERE rather than by the caller on purpose: compaction is
// LOSSY, and the marker in the transcript is how an operator learns why the
// assistant stopped remembering. That record must accompany the collapse, not
// depend on the retry afterwards succeeding.
func (s *Service) compactAfterContextLimit(ctx context.Context, tenantID, convID, sessionID, modelRef string, client scheduler.ChatTurnClient) (ok bool, detail string) {
	// THE TURN'S OWN CLIENT, passed in rather than resolved again. Resolving would be a second
	// adapter lookup that could in principle answer differently from the one that just failed, and
	// a resolution failure here would abandon a recovery that is standing on the failing turn's own
	// handle. The caller already has the right object.
	if client == nil {
		return false, "no chat client for this conversation"
	}
	compactor, isCompactor := client.(scheduler.ChatCompactor)
	if !isCompactor {
		// OPTIONAL by design (see scheduler.ChatCompactor): an adapter that
		// cannot compact says so, actionably, instead of appearing to succeed.
		return false, "the adapter for this conversation does not support compaction"
	}

	res, err := compactor.CompactConversationSession(ctx, scheduler.CompactConversationOpts{
		ConversationID: convID,
		SessionID:      sessionID,
		ModelRef:       modelRef,
		Reason:         contextLimitReason,
	})
	if err != nil {
		return false, err.Error()
	}
	if !res.Compacted {
		return false, res.Detail
	}

	// Advisory by contract: the collapse has already happened, so a failure to
	// write the marker is logged and never blocks the retry. WithoutCancel
	// because the notice describes work ALREADY DONE.
	if err := s.RecordCompactionNotice(context.WithoutCancel(ctx), scheduler.AskCompactNotice{
		ConversationID: convID,
		SessionID:      sessionID,
		Reason:         contextLimitReason,
		Detail:         res.Detail,
		ArchivePath:    res.ArchivePath,
		TokensBefore:   res.TokensBefore,
		TokensAfter:    res.TokensAfter,
	}); err != nil {
		s.log.Warn("ask orchicon: context-limit compaction notice was not recorded — the compaction stands, but the transcript will not show it",
			"conversation", convID, "error", err)
	}
	s.auditCompaction(ctx, tenantID, convID, contextLimitReason, res)
	return true, res.Detail
}

// isContextLimitFailure reports whether a turn's failure is the model refusing
// the prompt for its SIZE.
//
// It delegates to the ONE signature list (internal/orchicon's
// OversizePromptSignatures) that the native ladder already maintains, rather than
// keeping a shorter list here: the phrasings differ per provider and per gateway,
// and the group most easily lost by a second copy is the transport 413 family. A
// miss is not harmless — it means the operator is handed "reduce the length of
// the messages" instead of a conversation that simply continued.
func isContextLimitFailure(err error) bool {
	return orchicon.IsOversizePromptError(err)
}

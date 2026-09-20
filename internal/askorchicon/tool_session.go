package askorchicon

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// toolGetCurrentConversation reports THIS conversation's own session facts: its id, its mode, and the model_ref
// its turns actually resolve to — plus the SOURCE of that ref, so "why this one" is answerable.
//
// THE GAP IT CLOSES. Quick Work must ask, on every new dispatch, "the current model is <adapter/provider/model>,
// or a different one?" — and it could not. No tool exposed the running conversation's model_ref, so the only way
// to name the current model was to guess at it. A prompt that says "pin the worker to your model_ref" while
// giving the agent no way to read one produces exactly two behaviours, both wrong: invent a plausible ref, or
// silently substitute the tenant default and present it as "your model". This makes the fact READABLE instead of
// inferable.
//
// THE RESOLUTION IS DELIBERATELY THE TURN'S OWN (modelRefOrFallback, chat.go): the conversation's own ref when it
// carries one, else the tenant's DefaultAskOrchiconModel. Reporting anything else would name a model the session
// is demonstrably not running on — and the whole point is to offer the user the REAL one, by name, before it gets
// pinned into a worker that cannot fail over.
//
// It FAILS LOUD when no conversation is stamped rather than inventing one: answering "here is your model" from
// the tenant default for a session it could not identify would be confidently wrong, which is worse for the
// caller than a visible error.
func toolGetCurrentConversation(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	convID := askConversationFromContext(ctx)
	if convID == "" {
		return nil, fmt.Errorf("no conversation is stamped on this turn, so this session's own model_ref cannot be " +
			"resolved. Report that plainly rather than naming a model — a guess here gets pinned into a worker that " +
			"has no model failover")
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	conv, err := db.GetConversation(ctx, ttx.Tx, tenantID, convID)
	if err != nil {
		return nil, err
	}
	modelRef, source := conv.ModelRef, "conversation"
	if modelRef == "" {
		settings, err := db.GetTenantSettings(ctx, ttx.Tx, tenantID)
		if err != nil {
			return nil, err
		}
		modelRef, source = settings.DefaultAskOrchiconModel, "tenant_default"
	}
	out := map[string]any{
		"conversation_id":   conv.ID,
		"mode":              conv.Mode,
		"model_ref":         modelRef,
		"model_ref_source":  source,
		"model_ref_grammar": "adapter/provider/model (segment 1 must be a registered adapter kind)",
	}
	if modelRef == "" {
		out["note"] = "No model_ref is set on this conversation, on the tenant default, or anywhere else. Say that " +
			"plainly — do NOT build a worker with an empty model_ref."
	} else {
		out["note"] = "This is the model THIS conversation resolves to. Offer it BY NAME (all segments) as the " +
			"default for the dispatch, and let the user choose another."
	}
	return json.Marshal(out)
}

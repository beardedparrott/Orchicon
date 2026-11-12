package askorchicon

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// toolGetCurrentConversation reports THIS conversation's own session facts: its id, its mode, and
// the model_ref its turns actually resolve to — plus the SOURCE of that ref.
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

package askorchicon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"github.com/beardedparrott/orchicon/internal/workitem"
)

// toolListIdeas returns the Idea Cloud list (feature 5.1): idea-state work items with their
// automation provenance (spawned_by / spawned_by_run_id) plus a read-time SpawnedByTitle badge.
func toolListIdeas(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ProjectID string `json:"project_id"`
		Search    string `json:"search"`
		State     string `json:"state"`
		SortBy    string `json:"sort_by"`
		SortOrder string `json:"sort_order"`
		PageToken string `json:"page_token"`
		PageSize  int    `json:"page_size"`
	}
	if len(args) > 0 && string(args) != "null" {
		json.Unmarshal(args, &params)
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	// Fetch at most listCap+1 so truncation is detected without ever loading the whole Idea Cloud's fat
	// columns; a caller's page_size larger than that is clamped (the envelope note explains how.
	pageSize := params.PageSize
	if pageSize <= 0 || pageSize > listCap+1 {
		pageSize = listCap + 1
	}
	stateScope := domain.IdeaStateActive
	if strings.EqualFold(strings.TrimSpace(params.State), "rejected") {
		stateScope = domain.IdeaStateRejected
	}
	items, err := db.ListWorkItems(ctx, ttx.Tx, db.ListWorkItemsFilter{
		TenantID:       tenantID,
		ProjectID:      params.ProjectID,
		Search:         params.Search,
		SortBy:         params.SortBy,
		SortOrder:      params.SortOrder,
		PageSize:       pageSize,
		AfterID:        params.PageToken,
		IdeaScope:      domain.IdeaScopeOnly,
		IdeaStateScope: stateScope,
	})
	if err != nil {
		return nil, err
	}
	// Compact rows + the read-time SpawnedByTitle badge (the title of the
	// spawning recurring item), resolved with ONE batched query.
	out := make([]any, 0, len(items))
	spawnedIDs := make([]string, 0, len(items))
	idx := map[string]int{}
	for i, r := range items {
		m := compactWorkItem(r)
		if r.SpawnedByWorkItemID != nil && *r.SpawnedByWorkItemID != "" {
			m["SpawnedByWorkItemID"] = *r.SpawnedByWorkItemID
			if r.SpawnedByRunID != nil {
				m["SpawnedByRunID"] = *r.SpawnedByRunID
			}
			spawnedIDs = append(spawnedIDs, *r.SpawnedByWorkItemID)
			idx[*r.SpawnedByWorkItemID] = i
		}
		out = append(out, m)
	}
	if len(spawnedIDs) > 0 {
		titles, terr := db.SpawnedByTitles(ctx, ttx.Tx, tenantID, spawnedIDs)
		if terr != nil {
			return nil, terr
		}
		for id, title := range titles {
			if j, ok := idx[id]; ok {
				out[j].(map[string]any)["SpawnedByTitle"] = title
			}
		}
	}
	env := newCompactList(out, "get_work_item")
	if len(items) > listCap {
		env.setNextPage(items[listCap-1].ID)
	}
	return json.Marshal(env)
}

// PromoteIdea is the ONLY sanctioned path out of idea state — the generic update/delete/archive
// tools are gated.
func toolPromoteIdea(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.ID == "" {
		return nil, fmt.Errorf("id is required")
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, params.ID)
	if err != nil {
		return nil, err
	}
	if !workitem.IsIdeaStatus(current.Status) {
		return nil, fmt.Errorf("only idea-state work items can be promoted; approve it via PromoteIdea, or discard it via DismissIdea")
	}
	status := domain.WorkItemPending
	updated, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, params.ID, current.Version, db.UpdateWorkItemFields{Status: &status})
	if err != nil {
		return nil, err
	}
	if err := workitem.EnqueueWorkItemEvent(ctx, ttx.Tx, "work_item.promoted", updated); err != nil {
		return nil, err
	}
	if err := recordAudit(ctx, ttx.Tx, tenantID, "work_item.promoted", "work_item", updated.ID,
		workItemAuditJSON(current), workItemAuditJSON(updated)); err != nil {
		return nil, fmt.Errorf("audit work_item.promoted: %w", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(updated)
}

// toolDismissIdea discards an idea (feature 5.1): it transitions an idea-state work item to
// cancelled — the soft-delete/cancel terminal, consistent with DeleteWorkItem — via CAS.
func toolDismissIdea(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.ID == "" {
		return nil, fmt.Errorf("id is required")
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, params.ID)
	if err != nil {
		return nil, err
	}
	if !workitem.IsIdeaStatus(current.Status) {
		return nil, fmt.Errorf("only idea-state work items can be dismissed; discard it via DismissIdea, or approve it via PromoteIdea")
	}
	status := domain.WorkItemCancelled
	updated, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, params.ID, current.Version, db.UpdateWorkItemFields{Status: &status})
	if err != nil {
		return nil, err
	}
	if err := workitem.EnqueueWorkItemEvent(ctx, ttx.Tx, "work_item.dismissed", updated); err != nil {
		return nil, err
	}
	if err := recordAudit(ctx, ttx.Tx, tenantID, "work_item.dismissed", "work_item", updated.ID,
		workItemAuditJSON(current), workItemAuditJSON(updated)); err != nil {
		return nil, fmt.Errorf("audit work_item.dismissed: %w", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(updated)
}

// workItemAuditJSON builds a compact before/after snapshot for an idea transition audit row —
// enough to reconstruct the status flip (idea → pending/cancelled) and the version bump.
func workItemAuditJSON(w db.WorkItemRow) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"status":  w.Status,
		"version": w.Version,
	})
	return b
}

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/db"
)

// planeRegistry implements ToolRegistry for the plane channel
// (`orchicon-plane` MCP server). The `orchicon_plane_*` tools talk to the
// REAL instance's Connect API using a short-lived, role-scoped worker
// credential (ORCHICON_PLANE_TOKEN) — no Postgres DSN, no sandbox. The
// plane enforces RBAC authoritatively server-side; this registry only
// exposes the whitelisted tools (deny-by-default: the runtime lifecycle
// mints the credential only for published role-bound workers).
type planeRegistry struct {
	log   *slog.Logger
	runID string
	token string
	wi    apiv1connect.WorkItemServiceClient
	ai    apiv1connect.AIGatewayServiceClient
}

// NewPlaneRegistry returns a ToolRegistry backed by the plane's Connect
// API. url is the plane's public API base (ORCHICON_PLANE_URL); token is
// the run's scoped worker credential (ORCHICON_PLANE_TOKEN).
func NewPlaneRegistry(url, token string, log *slog.Logger) ToolRegistry {
	rt := &headerAuthTransport{base: http.DefaultTransport, token: token}
	hc := &http.Client{Transport: rt, Timeout: 60 * time.Second}
	return &planeRegistry{
		log:   log,
		runID: os.Getenv("ORCHICON_MCP_WORKFLOW_RUN_ID"),
		token: token,
		wi:    apiv1connect.NewWorkItemServiceClient(hc, url),
		ai:    apiv1connect.NewAIGatewayServiceClient(hc, url),
	}
}

// headerAuthTransport adds the Authorization bearer header to every
// request — the same header the plane's ResolveAuth middleware parses
// (internal/middleware/auth.go).
type headerAuthTransport struct {
	base  http.RoundTripper
	token string
}

func (t *headerAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(req)
}

var _ ToolRegistry = (*planeRegistry)(nil)

func (p *planeRegistry) List() []ToolDef {
	return []ToolDef{
		{
			Name:        "orchicon_plane_list_work_items",
			Description: "Lists work items in the REAL instance (the plane this run was created on) — role-scoped read of the current backlog. Use this to see what already exists instead of the sandbox tools. Returns a bounded compact list ({count, truncated, note, items}) — pass next_page_token to page through the rest, or branch to orchicon_plane_get_work_item for full detail.",
			Properties: map[string]propertySchema{
				"project_id": {Type: "string", Description: "Project ID filter (optional)"},
				"search":     {Type: "string", Description: "Free-text search across title and description (optional)"},
				"status":     {Type: "string", Description: "Status filter (optional): pending, scheduled, ready, assigned, running, checkpointing, succeeded, failed, cancelled, recovering"},
				"page_token": {Type: "string", Description: "Cursor for the next page — pass the previous response's next_page_token (default: first page)"},
			},
		},
		{
			Name:        "orchicon_plane_get_work_item",
			Description: "Gets a single work item by ID in the REAL instance (role-scoped read).",
			Properties: map[string]propertySchema{
				"id": {Type: "string", Description: "Work item ID"},
			},
			Required: []string{"id"},
		},
		{
			Name:        "orchicon_plane_create_work_item",
			Description: "Creates a work item in the REAL instance (role-scoped write). With the run context stamped, an automation-created item lands in IDEA state with provenance — the sanctioned automated write surface.",
			Properties: map[string]propertySchema{
				"title":               {Type: "string", Description: "Work item title"},
				"project_id":          {Type: "string", Description: "Project ID"},
				"kind":                {Type: "string", Description: "epic, feature, task, or subtask (default task)"},
				"parent_id":           {Type: "string", Description: "Parent work item ID (optional)"},
				"description":         {Type: "string", Description: "Markdown description (optional)"},
				"acceptance_criteria": {Type: "string", Description: "Markdown acceptance criteria (optional)"},
				"priority":            {Type: "string", Description: "Priority 1-5 (optional)"},
			},
			Mutating: true,
			Required: []string{"title", "project_id"},
		},
		{
			Name:        "orchicon_plane_get_usage",
			Description: "Gets token usage and cost for the REAL instance (role-scoped read).",
			Properties: map[string]propertySchema{
				"project_id": {Type: "string", Description: "Optional project filter"},
			},
		},
	}
}

func (p *planeRegistry) Execute(ctx context.Context, pool *db.Pool, name string, args json.RawMessage) (json.RawMessage, error) {
	switch name {
	case "orchicon_plane_list_work_items":
		return p.listWorkItems(ctx, args)
	case "orchicon_plane_get_work_item":
		return p.getWorkItem(ctx, args)
	case "orchicon_plane_create_work_item":
		return p.createWorkItem(ctx, args)
	case "orchicon_plane_get_usage":
		return p.getUsage(ctx, args)
	default:
		return nil, fmt.Errorf("unknown tool %q", name)
	}
}

func (p *planeRegistry) listWorkItems(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var a struct {
		ProjectID string `json:"project_id"`
		Search    string `json:"search"`
		Status    string `json:"status"`
		PageToken string `json:"page_token"`
	}
	_ = json.Unmarshal(raw, &a)
	req := &apiv1.ListWorkItemsRequest{ProjectId: a.ProjectID, Search: a.Search, PageSize: planeListCap + 1, PageToken: a.PageToken}
	if a.Status != "" {
		if st, ok := workItemStatusFromString(a.Status); ok {
			req.Status = &st
		}
	}
	resp, err := p.wi.ListWorkItems(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return compactPlaneWorkItems(resp.Msg.WorkItems)
}

// planeListCap caps how many items the plane channel's list tools return in
// one call. A list is for orienting — the worker branches to get_work_item
// for detail. Unbounded full-message lists were the "the list is HUGE"
// context bloat on the real backlog.
const planeListCap = 25

// compactPlaneWorkItems wraps a work-item page in the bounded envelope
// {count, truncated, note, items} with readable kind/status labels, so the
// model sees a consumable backlog and knows when to narrow or branch.
func compactPlaneWorkItems(items []*apiv1.WorkItem) (json.RawMessage, error) {
	rows := make([]map[string]any, 0, len(items))
	for _, it := range items {
		rows = append(rows, map[string]any{
			"id": it.Id, "title": it.Title,
			"kind":   workItemKindLabel(it.Kind),
			"status": workItemStatusLabel(it.Status),
			"priority": it.Priority,
		})
	}
	out := map[string]any{"count": len(rows), "items": rows}
	if len(rows) > planeListCap {
		out["count"] = planeListCap
		out["truncated"] = true
		out["items"] = rows[:planeListCap]
		out["next_page_token"] = items[planeListCap-1].Id
		out["note"] = fmt.Sprintf("%d more item(s) not shown — pass next_page_token to page through the rest, narrow with the search/status/project filters, or use orchicon_plane_get_work_item for full detail", len(rows)-planeListCap)
	}
	return json.Marshal(out)
}

func workItemKindLabel(k apiv1.WorkItemKind) string {
	return strings.ToLower(strings.TrimPrefix(k.String(), "WORK_ITEM_KIND_"))
}

func workItemStatusLabel(s apiv1.WorkItemStatus) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "WORK_ITEM_STATUS_"))
}

func (p *planeRegistry) getWorkItem(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var a struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &a)
	if a.ID == "" {
		return nil, fmt.Errorf("id is required")
	}
	resp, err := p.wi.GetWorkItem(ctx, connect.NewRequest(&apiv1.GetWorkItemRequest{Id: a.ID}))
	if err != nil {
		return nil, err
	}
	return json.Marshal(resp.Msg)
}

func (p *planeRegistry) createWorkItem(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var a struct {
		Title              string `json:"title"`
		ProjectID          string `json:"project_id"`
		Kind               string `json:"kind"`
		ParentID           string `json:"parent_id"`
		Description        string `json:"description"`
		AcceptanceCriteria string `json:"acceptance_criteria"`
		Priority           int32  `json:"priority"`
	}
	_ = json.Unmarshal(raw, &a)
	if a.Title == "" || a.ProjectID == "" {
		return nil, fmt.Errorf("title and project_id are required")
	}
	kind := apiv1.WorkItemKind_WORK_ITEM_KIND_TASK
	if a.Kind != "" {
		if k, ok := workItemKindFromString(a.Kind); ok {
			kind = k
		}
	}
	req := &apiv1.CreateWorkItemRequest{
		ProjectId:          a.ProjectID,
		ParentId:           a.ParentID,
		Kind:               kind,
		Title:              a.Title,
		Description:        a.Description,
		AcceptanceCriteria: a.AcceptanceCriteria,
		Priority:           a.Priority,
		RunContext:         p.runID,
	}
	resp, err := p.wi.CreateWorkItem(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return json.Marshal(resp.Msg)
}

func (p *planeRegistry) getUsage(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var a struct {
		ProjectID string `json:"project_id"`
	}
	_ = json.Unmarshal(raw, &a)
	resp, err := p.ai.GetUsage(ctx, connect.NewRequest(&apiv1.GetUsageRequest{ProjectId: a.ProjectID}))
	if err != nil {
		return nil, err
	}
	return json.Marshal(resp.Msg)
}

func workItemStatusFromString(s string) (apiv1.WorkItemStatus, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "pending":
		return apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, true
	case "scheduled":
		return apiv1.WorkItemStatus_WORK_ITEM_STATUS_SCHEDULED, true
	case "ready":
		return apiv1.WorkItemStatus_WORK_ITEM_STATUS_READY, true
	case "assigned":
		return apiv1.WorkItemStatus_WORK_ITEM_STATUS_ASSIGNED, true
	case "running":
		return apiv1.WorkItemStatus_WORK_ITEM_STATUS_RUNNING, true
	case "checkpointing":
		return apiv1.WorkItemStatus_WORK_ITEM_STATUS_CHECKPOINTING, true
	case "succeeded":
		return apiv1.WorkItemStatus_WORK_ITEM_STATUS_SUCCEEDED, true
	case "failed":
		return apiv1.WorkItemStatus_WORK_ITEM_STATUS_FAILED, true
	case "cancelled":
		return apiv1.WorkItemStatus_WORK_ITEM_STATUS_CANCELLED, true
	case "recovering":
		return apiv1.WorkItemStatus_WORK_ITEM_STATUS_RECOVERING, true
	}
	return apiv1.WorkItemStatus_WORK_ITEM_STATUS_UNSPECIFIED, false
}

func workItemKindFromString(s string) (apiv1.WorkItemKind, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "epic":
		return apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC, true
	case "feature":
		return apiv1.WorkItemKind_WORK_ITEM_KIND_FEATURE, true
	case "task":
		return apiv1.WorkItemKind_WORK_ITEM_KIND_TASK, true
	case "subtask":
		return apiv1.WorkItemKind_WORK_ITEM_KIND_SUBTASK, true
	}
	return apiv1.WorkItemKind_WORK_ITEM_KIND_UNSPECIFIED, false
}

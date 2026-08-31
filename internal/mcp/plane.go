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
	"github.com/beardedparrott/orchicon/internal/domain"
)

// planeRegistry implements ToolRegistry for the plane channel
// (`orchicon-plane` MCP server). The `orchicon_plane_*` tools talk to the
// REAL instance's Connect API using a short-lived, role-scoped worker
// credential (ORCHICON_PLANE_TOKEN) — no Postgres DSN, no sandbox. The
// plane enforces RBAC authoritatively server-side; this registry only
// exposes the whitelisted tools (deny-by-default: the runtime lifecycle
// mints the credential only for published role-bound workers).
type planeRegistry struct {
	log *slog.Logger
	// runContext is the owning workflow run's run_context JSONB captured
	// ONCE at registry construction: the automation provenance block a
	// recurring fire writes at fire time, injected by the CONTROL PLANE via
	// ORCHICON_RUN_CONTEXT at credential-mint time (runtime/lifecycle.go
	// mintPlaneCredential — the same trusted path that mints the scoped
	// token, the same model as the sandbox channel, feature 4.1 AC2).
	// A bare run ID is NOT a run_context: ProvenanceFromRunContext
	// unmarshal-fails on it and stamping silently no-ops (the bug this
	// replaces). The value is loaded at construction so the idea tools'
	// loud-no-op gate sees the SAME block every call (stale-binary runs —
	// the 04:12 failure — gate identically instead of reading a
	// mid-process-swapped env).
	runContext []byte
	token      string
	wi         apiv1connect.WorkItemServiceClient
	ai         apiv1connect.AIGatewayServiceClient
}

// NewPlaneRegistry returns a ToolRegistry backed by the plane's Connect
// API. url is the plane's public API base (ORCHICON_PLANE_URL); token is
// the run's scoped worker credential (ORCHICON_PLANE_TOKEN); the owning
// run's run_context JSONB arrives via ORCHICON_RUN_CONTEXT (empty for
// runs without one — creates then behave as plain human-path creates).
func NewPlaneRegistry(url, token string, log *slog.Logger) ToolRegistry {
	rt := &headerAuthTransport{base: http.DefaultTransport, token: token}
	hc := &http.Client{Transport: rt, Timeout: 60 * time.Second}
	return &planeRegistry{
		log:        log,
		runContext: loadRunContextOnce(),
		token:      token,
		wi:         apiv1connect.NewWorkItemServiceClient(hc, url),
		ai:         apiv1connect.NewAIGatewayServiceClient(hc, url),
	}
}

// loadRunContextOnce reads ORCHICON_RUN_CONTEXT once, up front. The
// sandbox channel does exactly this (internal/mcp/server.go loadRunContext
// at registration): reading at constructor time keeps the provenance block
// stable for the process lifetime so every idea-spawn decision is made
// against the same trusted input the token was minted with. Deliberately
// not logged — the block is automation metadata, not log fodder.
func loadRunContextOnce() []byte {
	v := strings.TrimSpace(os.Getenv("ORCHICON_RUN_CONTEXT"))
	if v == "" {
		return nil
	}
	return []byte(v)
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
			Description: "Lists work items in the REAL instance (the plane this run was created on) — role-scoped read of the current backlog. Use this to see what already exists instead of the sandbox tools. Returns a bounded compact list ({count, truncated, note, items}) — pass next_page_token to page through the rest, or branch to orchicon_plane_get_work_item for full detail. Set idea_scope=\"only\" to read the Idea Cloud (idea-state spawns) — required by the automation-spawn dedupe gate (idea-state items are hidden from the normal list).",
			Properties: map[string]propertySchema{
				"project_id": {Type: "string", Description: "Project ID filter (optional)"},
				"search":     {Type: "string", Description: "Free-text search across title and description (optional)"},
				"status":     {Type: "string", Description: "Status filter (optional): pending, scheduled, ready, assigned, running, checkpointing, succeeded, failed, cancelled, recovering"},
				"idea_scope": {Type: "string", Description: "\"only\" returns the Idea Cloud (idea-state items; the normal list hides them) (optional)"},
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
			Description: "Creates a work item in the REAL instance (role-scoped write). For automation idea spawning use orchicon_plane_create_idea_item instead — IDEA landing there is forced and self-verifying; this generic create lands by the server's own rules and reports the landed state only.",
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
		{
			Name:        "orchicon_plane_list_idea_items",
			Description: "Lists the Idea Cloud in the REAL instance (idea-state automation spawns, hidden from the normal list) — the MANDATORY dedupe gate before spawning: check here first so you never propose something that already exists. Returns the bounded compact envelope {count, items} with id/title/kind/status labels.",
			Properties: map[string]propertySchema{
				"project_id": {Type: "string", Description: "Project ID filter (optional)"},
				"search":     {Type: "string", Description: "Free-text search across title and description (optional)"},
			},
		},
		{
			Name:        "orchicon_plane_create_idea_item",
			Description: "SPAWNS an idea-state work item in the REAL instance (the sanctioned automation write surface). IDEA landing is forced by the tool, not by parameters — provenance is stamped from the run's trusted run_context, never from call arguments. The response envelope self-verifies: it reports landed_status (\"idea\"), idea_state: true, and spawned provenance. A missing or non-idea run context makes the spawn REFUSE loudly (never a silent plain-pending landing).",
			Properties: map[string]propertySchema{
				"title":               {Type: "string", Description: "Work item title"},
				"project_id":          {Type: "string", Description: "Project ID"},
				"kind":                {Type: "string", Description: "epic, feature, task, or subtask (default task)"},
				"parent_id":           {Type: "string", Description: "Parent work item ID (optional; only 'epic' may be top-level)"},
				"description":         {Type: "string", Description: "Markdown description (optional)"},
				"acceptance_criteria": {Type: "string", Description: "Markdown acceptance criteria (optional)"},
				"priority":            {Type: "string", Description: "Priority 1-5 (optional)"},
			},
			Mutating: true,
			Required: []string{"title", "project_id"},
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
	case "orchicon_plane_list_idea_items":
		return p.listIdeaItems(ctx, args)
	case "orchicon_plane_create_idea_item":
		return p.createIdeaItem(ctx, args)
	default:
		return nil, fmt.Errorf("unknown tool %q", name)
	}
}

func (p *planeRegistry) listWorkItems(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var a struct {
		ProjectID string `json:"project_id"`
		Search    string `json:"search"`
		Status    string `json:"status"`
		IdeaScope string `json:"idea_scope"`
		PageToken string `json:"page_token"`
	}
	_ = json.Unmarshal(raw, &a)
	req := &apiv1.ListWorkItemsRequest{ProjectId: a.ProjectID, Search: a.Search, PageSize: planeListCap + 1, PageToken: a.PageToken}
	if a.Status != "" {
		if st, ok := workItemStatusFromString(a.Status); ok {
			req.Status = &st
		}
	}
	// idea_scope="only" reads the Idea Cloud (idea-state items are hidden
	// from the normal list server-side) — the plane surface of the
	// automation-spawn dedupe gate ("check the Idea Cloud first").
	if strings.TrimSpace(strings.ToLower(a.IdeaScope)) == "only" {
		req.IdeaScope = apiv1.IdeaScope_IDEA_SCOPE_ONLY_IDEA
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
		RunContext:         string(p.runContext),
	}
	resp, err := p.wi.CreateWorkItem(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return compactPlaneWorkItemCreated(resp.Msg)
}

// compactPlaneWorkItemCreated wraps the create response in a compact
// labeled envelope. The raw proto JSON marshals status as a bare enum
// NUMBER (`status: 1` = pending), which a worker can misread as
// confirmation of a different landing state (the concrete failure that
// motivated this branch: a synthesizer believed status=1 meant IDEA and
// reported success while every item landed plain pending). Labeled
// strings + explicit landed-state fields make the result impossible to
// misinterpret.
func compactPlaneWorkItemCreated(msg *apiv1.CreateWorkItemResponse) (json.RawMessage, error) {
	wi := msg.GetWorkItem()
	if wi == nil {
		return json.Marshal(map[string]any{"error": "create returned no work item"})
	}
	out := map[string]any{
		"id":            wi.GetId(),
		"title":         wi.GetTitle(),
		"kind":          workItemKindLabel(wi.GetKind()),
		"status":        workItemStatusLabel(wi.GetStatus()),
		"landed_status": workItemStatusLabel(wi.GetStatus()),
		"parent_id":     wi.GetParentId(),
		"priority":      wi.GetPriority(),
		"spawned_by":    wi.GetSpawnedBy(),
		"spawned_run":   wi.GetSpawnedByRunId(),
		"idea_state":    wi.GetStatus() == apiv1.WorkItemStatus_WORK_ITEM_STATUS_IDEA,
	}
	return json.Marshal(out)
}

// ideaProvenance classifies the run context into what the idea tools
// require: a parsed provenance block with the recurrence's outputs_mode.
// Missing keys -> zero strings (a run without a provenance block is a
// plain run, not an error).
func ideaProvenance(rc []byte) (spawnedBy, runID, mode string) {
	return db.ProvenanceFromRunContext(rc)
}

// refuseIdeaSpawn is the loud-no-op gate the idea tools enforce BEFORE any
// write: it fails the call with the exact reason, so a spawn can NEVER
// silently land as a plain pending item (the failure mode that shipped
// twice — the bare-run-ID relay and the 04:12 stale-sidecar run whose
// create reported a bare `status: 1` with no idea_state field). Compare
// with the old model: server-side stamping no-op'd silently on a missing
// block and the worker had no signal at all.
func refuseIdeaSpawn(rc []byte) error {
	spawnedBy, _, mode := ideaProvenance(rc)
	if spawnedBy == "" {
		return fmt.Errorf("idea spawn refused: this run's run_context carries no automation provenance block " +
			"(missing spawned_by — the credential mint did not relay ORCHICON_RUN_CONTEXT, " +
			"or this runtime container is serving a STALE binary from before the idea tools existed). " +
			"Record this as a FACTS LEARNED line, do NOT report success, and ship the manifest in the brief for UI spawning instead")
	}
	if mode != domain.RecurringOutputsIdea {
		return fmt.Errorf("idea spawn refused: this run's provenance outputs_mode is %q, not %q — a non-idea recurrence cannot spawn idea items",
			mode, domain.RecurringOutputsIdea)
	}
	return nil
}

func (p *planeRegistry) listIdeaItems(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var a struct {
		ProjectID string `json:"project_id"`
		Search    string `json:"search"`
	}
	_ = json.Unmarshal(raw, &a)
	resp, err := p.wi.ListWorkItems(ctx, connect.NewRequest(&apiv1.ListWorkItemsRequest{
		ProjectId: a.ProjectID,
		Search:    a.Search,
		PageSize:  planeListCap + 1,
		IdeaScope: apiv1.IdeaScope_IDEA_SCOPE_ONLY_IDEA,
	}))
	if err != nil {
		return nil, err
	}
	return compactPlaneWorkItems(resp.Msg.WorkItems)
}

func (p *planeRegistry) createIdeaItem(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	// GATE FIRST, before any network write: no provenance / wrong outputs
	// mode -> the spawn refuses with the reason. A stale sidecar binary
	// (no idea tools at all) fails the other direction — the worker sees
	// "unknown tool" instead of a wrong landing.
	if err := refuseIdeaSpawn(p.runContext); err != nil {
		return nil, err
	}
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
	// Provenance rides the RELAYED run_context — the same trusted block the
	// gate checked — never call arguments. The server's stamping path
	// (workitem service CreateWorkItem -> ApplyAutomationProvenance via
	// msg.RunContext) lands the item in IDEA state with spawned_by /
	// spawned_by_run_id exactly like the human-path sandbox channel.
	req := &apiv1.CreateWorkItemRequest{
		ProjectId:          a.ProjectID,
		ParentId:           a.ParentID,
		Kind:               kind,
		Title:              a.Title,
		Description:        a.Description,
		AcceptanceCriteria: a.AcceptanceCriteria,
		Priority:           a.Priority,
		RunContext:         string(p.runContext),
	}
	resp, err := p.wi.CreateWorkItem(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	// Trust-but-verify the landed state: the gate proves the relayed
	// context was spawn-worthy, but the SERVER decides the final status.
	// If the landing is not idea despite a spawn-worthy context, the
	// response is an error envelope, never a success-shaped one.
	if wi := resp.Msg.GetWorkItem(); wi == nil || wi.GetStatus() != apiv1.WorkItemStatus_WORK_ITEM_STATUS_IDEA {
		return compactIdeaEnvelopeError(resp.Msg.GetWorkItem())
	}
	return compactPlaneWorkItemCreated(resp.Msg)
}

// compactIdeaEnvelopeError wraps a create response whose landed state is
// NOT idea despite the gate passing: the server-side stamp silently no-op'd
// (or an old plane binary mishandled the relayed run_context). The worker
// must never misread this as success; the error names the exact mismatch.
func compactIdeaEnvelopeError(wi *apiv1.WorkItem) (json.RawMessage, error) {
	if wi == nil {
		return json.Marshal(map[string]any{"error": "create returned no work item"})
	}
	return json.Marshal(map[string]any{
		"error": "IDEA landing NOT confirmed — the item landed as " + workItemStatusLabel(wi.GetStatus()) +
			" with spawned_by=" + wi.GetSpawnedBy() + ", spawned_run=" + wi.GetSpawnedByRunId() +
			". This is a platform bug (server-side stamp did not apply): record it as a FACTS LEARNED line " +
			"and use orchicon_plane_get_work_item to inspect the landed item.",
		"idea_state": false,
		"landed_status": workItemStatusLabel(wi.GetStatus()),
	})
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

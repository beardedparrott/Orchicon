package askorchicon

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/beardedparrott/orchicon/internal/db"
)

// listCap caps the number of rows any list tool returns in one call. A list
// is for orienting and choosing — the model branches to the corresponding
// get_* tool (or narrows with search/status filters) for detail. Returning
// hundreds of FULL rows (a work item row carries description, acceptance
// criteria, budgets, context files, worker refs…) blows the context for no
// value: the "the list is HUGE, let me search for a specific item" failure
// mode Ask Orchicon and workers hit on the real backlog.
const listCap = 25

// compactList is the envelope every list tool returns: a bounded set of
// compact rows plus an explicit note when more exist, so the model can
// decide to narrow or branch instead of assuming it saw everything.
type compactList struct {
	Count     int    `json:"count"`
	Truncated bool   `json:"truncated"`
	Note      string `json:"note,omitempty"`
	Items     []any  `json:"items"`
	// NextPageToken is carried through for tools whose underlying query
	// supports cursor pagination (secrets); empty when absent.
	NextPageToken string `json:"next_page_token,omitempty"`
}

// newCompactList wraps rows in the compact envelope, capping at listCap.
// detailTool names the get_* tool to branch to for full detail ("" when no
// detail tool exists — the note then points at the list's own filters).
func newCompactList(rows []any, detailTool string) compactList {
	out := compactList{Count: len(rows), Items: rows}
	if len(rows) > listCap {
		out = compactList{
			Count:     listCap,
			Truncated: true,
			Items:     rows[:listCap],
		}
		if detailTool != "" {
			out.Note = fmt.Sprintf("%d more item(s) not shown — narrow with search/status filters or use %s for full detail", len(rows)-listCap, detailTool)
		} else {
			out.Note = fmt.Sprintf("%d more item(s) not shown — narrow with the search/status/project filters", len(rows)-listCap)
		}
	}
	return out
}

// setNextPage records the cursor for the next page (the last shown row's ID)
// and appends a paging hint to the note. It is a no-op on a NON-truncated
// list: if the whole result fit in the page there is no next page, so the
// envelope must never carry a next_page_token. (`list_secrets` sets the
// token directly — its cursor comes from the secrets service's own paging,
// which can be non-empty independently of this envelope's cap.)
func (c *compactList) setNextPage(token string) {
	if token == "" || !c.Truncated {
		return
	}
	c.NextPageToken = token
	c.Note = strings.TrimSpace(c.Note) + " Or pass next_page_token for the next page."
}

// --- compact row builders -------------------------------------------------
//
// Each returns the SAME PascalCase keys as the full get_* row output (the
// db rows marshal without tags), so a compact list row reads as a trimmed
// get_* result — the model can branch to get_<entity> for everything else.
// Heavy fields (descriptions, acceptance criteria, budgets, context files,
// run_context, conversation, before/after snapshots, …) never appear here.

func compactWorkItem(r db.WorkItemRow) map[string]any {
	return map[string]any{
		"ID": r.ID, "Title": r.Title, "Kind": r.Kind, "Status": r.Status,
		"Priority": r.Priority, "ProjectID": r.ProjectID,
	}
}

func compactProject(r db.ProjectRow) map[string]any {
	// Goals is a JSON-encoded string on the row; decode it so the prompt
	// context injection (fetchProjectContext) gets real text, not base64.
	var goals string
	if len(r.Goals) > 0 {
		_ = json.Unmarshal(r.Goals, &goals)
	}
	return map[string]any{
		"ID": r.ID, "Name": r.Name, "Slug": r.Slug, "Status": r.Status,
		"ProjectDir": r.ProjectDir, "Goals": goals,
	}
}

func compactWorker(r db.WorkerRow) map[string]any {
	return map[string]any{
		"ID": r.ID, "Name": r.Name, "Slug": r.Slug, "Status": r.Status,
		"RoleRef": r.RoleRef,
	}
}

func compactWorkflow(r db.WorkflowRow) map[string]any {
	return map[string]any{
		"ID": r.ID, "Name": r.Name, "Type": r.Type, "Status": r.Status,
		"ProjectID": r.ProjectID,
	}
}

func compactWorkflowRun(r db.WorkflowRunRow) map[string]any {
	return map[string]any{
		"ID": r.ID, "WorkflowID": r.WorkflowID, "WorkItemID": r.WorkItemID,
		"Status": r.Status, "CurrentStep": r.CurrentStep, "StartedAt": r.StartedAt,
	}
}

func compactExecution(r db.ExecutionRow) map[string]any {
	return map[string]any{
		"ID": r.ID, "Status": r.Status, "HealthState": r.HealthState,
		"TaskName": r.TaskName, "WorkerName": r.WorkerName,
		"WorkflowRunID": r.WorkflowRunID, "StartedAt": r.StartedAt,
	}
}

func compactPolicy(r db.PolicyRow) map[string]any {
	return map[string]any{"ID": r.ID, "Name": r.Name, "Status": r.Status}
}

func compactRecovery(r db.RecoveryExecutionRow) map[string]any {
	return map[string]any{
		"ID": r.ID, "Status": r.Status, "CurrentStep": r.CurrentStep,
		"Level": r.Level, "ProjectID": r.ProjectID, "TaskID": r.TaskID,
		"TriggeredAt": r.TriggeredAt,
	}
}

func compactRuntimeImage(r db.RuntimeImageRow) map[string]any {
	return map[string]any{
		"ID": r.ID, "Name": r.Name, "Slug": r.Slug, "Tag": r.Tag,
		"Status": r.Status, "Version": r.Version,
	}
}

func compactUsage(r db.UsageRecordRow) map[string]any {
	return map[string]any{
		"ID": r.ID, "OccurredAt": r.OccurredAt, "Model": r.Model,
		"Provider": r.Provider, "TotalTokens": r.TotalTokens,
		"CostUSD": r.CostUSD, "TaskTitle": r.TaskTitle,
		"WorkerName": r.WorkerName, "ProjectID": r.ProjectID,
	}
}
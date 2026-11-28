package askorchicon

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/beardedparrott/orchicon/internal/db"
)

// listCap caps the number of rows any list tool returns in one call.
const listCap = 25

// compactList is the envelope every list tool returns: a bounded set of compact rows plus an
// explicit note when more exist.
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
// newCompactList trims a row slice to listCap.
//
// CALLERS MUST FETCH listCap+1 ROWS, and the note states WHICH case they are in, because a note that
// can only say one thing is not evidence. It used to assume the +1 probe row was always there and
// always print "%d more item(s) not shown" — so a filter matching 60 rows and one matching 26 returned
// the SAME count (25) and the SAME note ("1 more item(s) not shown"), and a reader could not tell
// whether a filter had narrowed anything. That is what made an unfiltered and a filtered call look
// identical when checking whether a filter was wired.
//
// Two distinct notes now:
//   - the probe row was returned: there IS more, and the count is exact;
//   - no probe row: exactly listCap matched, and the note says so rather than implying more.
func newCompactList(rows []any, detailTool string) compactList {
	if len(rows) <= listCap {
		return compactList{Count: len(rows), Items: rows}
	}
	more := len(rows) - listCap
	note := ""
	if detailTool != "" {
		note = fmt.Sprintf("%d more item(s) not shown — narrow with search/status filters or use %s for full detail", more, detailTool)
	} else {
		note = fmt.Sprintf("%d more item(s) not shown — narrow with the search/status/project filters", more)
	}
	return compactList{Count: listCap, Truncated: true, Items: rows[:listCap], Note: note}
}

// setNextPage records the cursor for the next page (the last shown row's ID) and appends a paging
// hint to the note.
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
	// GitStrategy and RepoSlug are carried on the COMPACT row on purpose.
	row := map[string]any{
		"ID": r.ID, "Name": r.Name, "Slug": r.Slug, "Status": r.Status,
		"ProjectDir": r.ProjectDir, "Goals": goals, "GitStrategy": r.GitStrategy,
	}
	if r.RepoSlug != nil {
		row["RepoSlug"] = *r.RepoSlug
	}
	return row
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

// compactUsage trims a usage record for the compact list.
//
// IT NOW CARRIES THE ATTRIBUTION KEYS (ExecutionID, TaskID, WorkflowRunID, SessionID). Without them a
// reader that received a page of rows had NO WAY to say which execution a row belonged to, so an agent
// asked "how many tokens did execution X use?" could only see a flat recent-activity feed and sum or
// eyeball it. That is how a per-execution question gets answered with an unrelated number.
func compactUsage(r db.UsageRecordRow) map[string]any {
	return map[string]any{
		"ID": r.ID, "OccurredAt": r.OccurredAt, "Model": r.Model,
		"Provider": r.Provider, "TotalTokens": r.TotalTokens,
		"CostUSD": r.CostUSD, "TaskTitle": r.TaskTitle,
		"WorkerName": r.WorkerName, "ProjectID": r.ProjectID,
		// Attribution: the keys that let a row be tied to the thing it paid for.
		"ExecutionID": r.ExecutionID, "TaskID": r.TaskID,
		"WorkflowRunID": r.WorkflowRunID, "SessionID": r.SessionID,
		"AdapterKind": r.AdapterKind, "Billing": r.Billing,
	}
}

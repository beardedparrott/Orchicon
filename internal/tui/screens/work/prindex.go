package work

// prindex.go — THE PER-ITEM PR SURFACE of the Work screen.
//
// WHY IT IS A SEPARATE FETCH RATHER THAN A FIELD ON THE ITEM. The operator wanted the work-item list to show
// whether a run's PR had merged, and the details pane to carry the URL. The work item itself cannot answer that:
// the PR is captured into the RUN (the DevOps worker authors PR_URL/PR_STATE, the platform mirrors them onto the
// execution), and the item's proto carries no PR fields at all. The GUI already solves this the same way —
// `useListExecutions({projectId})` grouped by `taskId`, deliberately with NO new RPC — so this mirrors that
// rather than inventing a second source of truth for the same fact.
//
// THE JOIN IS `execution.task_id -> work_item.id`, and the page's item ids are the filter: an execution for a
// work item that is not on the loaded page is ignored, so the index stays proportional to the screen.

import (
	"context"

	"connectrpc.com/connect"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// executionPageSize is the page this screen asks ListExecutions for.
//
// IT IS A BOUND, AND AN HONEST ONE. The join needs every run whose item is on screen, but ListExecutions has no
// "these task ids" filter — only project/task/status/workflow-run — and this screen lists work items TENANT-WIDE,
// so there is no project to narrow by either. A tenant whose execution history exceeds this page therefore keeps
// the PR mark on its RECENT items and loses it on the oldest ones, which is the honest direction for the failure
// to point: a missing mark reads as "no PR recorded" rather than asserting a merge that did not happen. 1000
// matches the work-item page, so both halves of this screen agree on how much history they are describing.
const executionPageSize = 1000

// prRun is ONE run's PR surface: the authored URL and its state (open/merged/draft/closed).
//
// The URL is required for an entry to exist at all — the platform captures pr_url and pr_state together, and a
// state with no URL is not a link anyone can follow.
type prRun struct {
	url   string
	state string
}

// mergedRunState is the ONE state that means the change actually landed. The vocabulary is the platform's own
// (frontend/src/lib/pr.ts prStateLabel), so the two clients cannot disagree about which word means merged.
const mergedRunState = "merged"

// prIndex maps a work item id to that item's runs' PR surfaces, NEWEST FIRST.
//
// The order is the contract of the fetch (SortOrder desc by created_at): it is what "the latest run's PR" means
// for the details pane. A plain map of id -> single entry would have to pick a winner at write time and could not
// express "any run merged" at all.
type prIndex map[string][]prRun

// merged reports whether ANY of the item's runs produced a merged PR.
//
// ANY rather than the latest, which is the operator's explicit rule: "For PR I want the PR Merged if any has
// it." The distinction is real — a fix-up run after a failed PR still leaves the item's change merged — and it is
// the reason the index keeps a list instead of a single "last" value.
func (p prIndex) merged(id string) bool {
	for _, r := range p[id] {
		if r.state == mergedRunState {
			return true
		}
	}
	return false
}

// link resolves the URL and state the details pane shows: a MERGED run's PR when there is one (the change that
// landed, which is what an operator goes looking for), else the newest run that has a PR at all.
//
// Returns empty strings when the item has no PR on record, so the caller renders NO field rather than a blank
// one — the same discipline the workflow-run field already follows.
func (p prIndex) link(id string) (url, state string) {
	runs := p[id]
	for _, r := range runs {
		if r.state == mergedRunState {
			return r.url, r.state
		}
	}
	if len(runs) > 0 {
		return runs[0].url, runs[0].state
	}
	return "", ""
}

// prDisplay renders a PR link for the details pane: the URL, with its state beside it when the platform recorded
// one. The URL is the point (the operator's "add a field at the top of the details pane that shows the URL"); the
// state is carried because a bare URL does not say whether the change landed, and the pane is where that is read.
func prDisplay(url, state string) string {
	if state == "" || state == "none" {
		return url
	}
	return url + " (" + state + ")"
}

// fetchItemPRs builds the PR index for the given page of work items.
//
// BEST EFFORT BY CONSTRUCTION, and this is deliberate rather than lax: the PR mark is DECORATION on a list whose
// real content is the items. A failed or unimplemented executions call must leave the work-item list exactly as it
// was, so the error is swallowed and an empty index is returned — which renders no marks, i.e. the previous
// behaviour — rather than failing the list the operator asked for. (The tests rely on this too: their plane does
// not serve executions.)
func (m *Model) fetchItemPRs(ctx context.Context, items []*apiv1.WorkItem) prIndex {
	if len(items) == 0 {
		return nil
	}
	known := make(map[string]bool, len(items))
	for _, w := range items {
		if id := w.GetId(); id != "" {
			known[id] = true
		}
	}
	resp, err := m.cl.Executions.ListExecutions(ctx, connect.NewRequest(&apiv1.ListExecutionsRequest{
		PageSize:  executionPageSize,
		SortBy:    "created_at",
		SortOrder: "desc",
	}))
	if err != nil {
		return nil
	}
	idx := prIndex{}
	for _, e := range resp.Msg.GetExecutions() {
		id, url := e.GetTaskId(), e.GetPrUrl()
		if id == "" || url == "" || !known[id] {
			continue
		}
		idx[id] = append(idx[id], prRun{url: url, state: e.GetPrState()})
	}
	if len(idx) == 0 {
		return nil
	}
	return idx
}

// itemPRLink is the details pane's read of the index, taken under the mutex because the pane is rendered off the
// update loop while the index is written by a fetch that also runs off it.
func (m *Model) itemPRLink(id string) (url, state string) {
	m.viewMu.Lock()
	idx := m.runsByItem
	m.viewMu.Unlock()
	return idx.link(id)
}

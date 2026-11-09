package work

// prindex.go — THE PER-ITEM PR SURFACE of the Work screen.

import (
	"context"

	"connectrpc.com/connect"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// executionPageSize is the page this screen asks ListExecutions for.
const executionPageSize = 1000

// prRun is ONE run's PR surface: the authored URL and its state (open/merged/draft/closed).
type prRun struct {
	url   string
	state string
}

// mergedRunState is the ONE state that means the change actually landed. The vocabulary is the platform's own
// (frontend/src/lib/pr.ts prStateLabel), so the two clients cannot disagree about which word means merged.
const mergedRunState = "merged"

// prIndex maps a work item id to that item's runs' PR surfaces, NEWEST FIRST.
type prIndex map[string][]prRun

// merged reports whether ANY of the item's runs produced a merged PR.
func (p prIndex) merged(id string) bool {
	for _, r := range p[id] {
		if r.state == mergedRunState {
			return true
		}
	}
	return false
}

// link resolves the URL and state the details pane shows: a MERGED run's PR when there is one (the
// change that landed, which is what an operator goes looking for).
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

// prDisplay renders a PR link for the details pane: the URL, with its state beside it when the
// platform recorded one.
func prDisplay(url, state string) string {
	if state == "" || state == "none" {
		return url
	}
	return url + " (" + state + ")"
}

// fetchItemPRs builds the PR index for the given page of work items.
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

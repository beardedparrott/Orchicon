package work

// bulk_set.go — the BULK counterpart of the single-item workflow/runtime-image form.
//
// WHY THIS EXISTS. A work item with no workflow binding cannot run: the reconciler backstops and
// the status transitions refuse. That turns "bind a workflow" from a convenience into the only way
// to unblock an item — and it can strand a whole backlog that is bound to nothing. Binding a
// selection one modal at a time is the tedium this removes.
//
// WHY `workflow_id` AND `runtime_image` TOGETHER. A step's container and the workflow that drives
// it are a pair: they are chosen together when building sequential work items. Offering one without
// the other would leave the operator doing half the job per item.
//
// WHY A SCALAR FIELD IS DIFFERENT FROM STATUS/REASSIGNMENT. bulkItemActions' comment explains why
// bulk STATUS change and WORKER ASSIGNMENT are withheld: each carries its own acceptance review and
// worker binding, so they would need a form per item. That reasoning does NOT reach a scalar field:
// a workflow binding is ONE picker, ONE value, identical for every selected item — exactly the
// shape BulkUpdateWorkerModel already proved out for the Workers pane.
//
// NO PROTO CHANGE AND NO NEW RPC. `UpdateWorkItemRequest.workflow_id` (field 15) and
// `.runtime_image` (field 19) are both `optional`: an UNSET field is left unchanged and `empty` is
// the documented unbind/base-image value. So the write is N sequential UpdateWorkItem calls, exactly
// like the existing bulk archive/delete, and clearing needs no extra mechanism.
//
// THE INDEPENDENCE GUARANTEE (AC2) IS STRUCTURAL, NOT HOPED FOR: an untouched field is left as a nil
// pointer, so it is OMITTED from the request entirely. Sending a zero value for an untouched field
// would silently unbind it — that is the one mistake this file must never make.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

const (
	// formBulkSetItem is the modal form mode for the bulk picker.
	formBulkSetItem = "item-bulk-set"
	// keyBulkSet is the action-bar key + chord for the bulk set. `W` is unused on this screen
	// (see handleKey's case list: n e d s b v T Z O / + - y a R x ctrl+x esc).
	keyBulkSet = "W"
	// bulkSetSkip is the "leave unchanged" sentinel. It is deliberately a value no real workflow
	// id or runtime-image tag can equal, so the field can be OMITTED from the request (nil
	// pointer) rather than sent as a zero value.
	bulkSetSkip = "\x00skip"
	// bulkSetClear is the documented clearing value: empty workflow_id = unbind; empty
	// runtime_image = base image. Per the proto, that is the whole clearing mechanism.
	bulkSetClear = ""
)

// workflowStepsNonEmpty reports whether a WorkflowVersion.steps JSON document holds at least one
// step. Steps is a JSON array of Step messages (workflow.proto:50), so "[]" and "" are both empty.
func workflowStepsNonEmpty(steps string) bool {
	steps = strings.TrimSpace(steps)
	if steps == "" {
		return false
	}
	var arr []json.RawMessage
	if err := json.Unmarshal([]byte(steps), &arr); err != nil {
		// A malformed steps document cannot be shown to be runnable: refuse it rather than offer a
		// target that will fail at schedule time. Over-restriction is the safe direction.
		return false
	}
	return len(arr) > 0
}

// workflowIsRunnable is the predicate the SERVER applies — but only at SCHEDULE time
// (ValidateSequenceSubtree, internal/workitem/validate.go: status PUBLISHED|DEPRECATED AND the
// leaf's workflow has >= 1 step).
//
// UpdateWorkItem performs NO bind-time validation of workflow_id, so a bulk bind of a non-runnable
// template would land silently and fail later, when the item is scheduled. This picker therefore
// applies the schedule-time predicate client-side and EXCLUDES non-runnable templates.
func workflowIsRunnable(w *apiv1.Workflow, v *apiv1.WorkflowVersion) bool {
	switch w.GetStatus() {
	case apiv1.WorkflowStatus_WORKFLOW_STATUS_PUBLISHED,
		apiv1.WorkflowStatus_WORKFLOW_STATUS_DEPRECATED:
	default:
		return false
	}
	return workflowStepsNonEmpty(v.GetSteps())
}

// loadRunnableWorkflows lists the tenant's workflow TEMPLATES and keeps only the runnable ones
// (see workflowIsRunnable), counting the dropped ones so the picker can say how many it hid.
//
// The runnable predicate needs the steps, and the `Workflow` message carries none — so each
// candidate costs one GetWorkflow for its latest_version.
func loadRunnableWorkflows(ctx context.Context, cl *client.Clients) (opts []workflowOpt, hidden int, err error) {
	resp, err := cl.Workflows.ListWorkflows(ctx, connect.NewRequest(&apiv1.ListWorkflowsRequest{
		Type:     "template",
		PageSize: 100,
	}))
	if err != nil {
		return nil, 0, err
	}
	for _, w := range resp.Msg.GetWorkflows() {
		if w.GetStatus() != apiv1.WorkflowStatus_WORKFLOW_STATUS_PUBLISHED &&
			w.GetStatus() != apiv1.WorkflowStatus_WORKFLOW_STATUS_DEPRECATED {
			hidden++
			continue
		}
		gr, err := cl.Workflows.GetWorkflow(ctx, connect.NewRequest(&apiv1.GetWorkflowRequest{Id: w.GetId()}))
		if err != nil {
			hidden++
			continue
		}
		if !workflowStepsNonEmpty(gr.Msg.GetLatestVersion().GetSteps()) {
			hidden++
			continue
		}
		opts = append(opts, workflowOpt{ID: w.GetId(), Name: w.GetName()})
	}
	return opts, hidden, nil
}

// prepBulkSet loads the picker's option lists (runnable workflows + the runtime images the forms
// already use) and snapshots the marked ids. It is the bulk counterpart of prepEditItem.
func (m *Model) prepBulkSet() tea.Cmd {
	// A write path must never PANIC on a missing client — same guard shape as bulkItemActions.
	if m.cl == nil || m.cl.Workflows == nil || m.cl.WorkItems == nil {
		m.formLoading = false
		m.notice = "no work-item client"
		return nil
	}
	m.formLoading = true
	// Snapshot the selection NOW: the modal owns the keys while it is up, but the mark set could
	// still be cleared by a background reconciliation, and the write must target what the operator
	// confirmed.
	m.bulkSetIDs = m.Base.BulkIDs()
	cl := m.cl
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		msg := itemFormMsg{mode: formBulkSetItem}
		wfs, hidden, err := loadRunnableWorkflows(ctx, cl)
		if err != nil {
			msg.err = err
			return msg
		}
		msg.workflows = wfs
		msg.hiddenWorkflows = hidden
		// The runtime-image list is the SAME source the create/edit forms use: value = the tag the
		// request carries.
		if lr, err := cl.Images.ListRuntimeImages(ctx, connect.NewRequest(&apiv1.ListRuntimeImagesRequest{PageSize: 100})); err == nil {
			for _, img := range lr.Msg.GetRuntimeImages() {
				msg.images = append(msg.images, kit2.Option{
					Value: img.GetTag(),
					Label: img.GetName() + " (" + img.GetTag() + ")",
				})
			}
		}
		return msg
	}
}

// newBulkSetForm builds the bulk picker: ONE workflow picker and ONE runtime-image picker, each
// defaulting to "leave unchanged" so a workflow-only set cannot touch runtime images (and vice
// versa). Both option lists are the same sources the single-item forms already use.
func (m *Model) newBulkSetForm(wfs []workflowOpt, imgs []kit2.Option) *kit2.Form {
	wfOpts := []kit2.Option{
		{Value: bulkSetSkip, Label: "— leave workflow unchanged —"},
		{Value: bulkSetClear, Label: "— clear (unbind) —"},
	}
	names := make(map[string]string, len(wfs))
	for _, w := range wfs {
		wfOpts = append(wfOpts, kit2.Option{Value: w.ID, Label: w.Name})
		names[w.ID] = w.Name
	}
	imgOpts := []kit2.Option{
		{Value: bulkSetSkip, Label: "— leave image unchanged —"},
		{Value: bulkSetClear, Label: "— clear (base image) —"},
	}
	imgOpts = append(imgOpts, imgs...)

	n := len(m.bulkSetIDs)
	items := "items"
	if n == 1 {
		items = "item"
	}
	f := kit2.NewForm("Set workflow & runtime image on "+strconv.Itoa(n)+" "+items,
		// `Initial:` is MANDATORY on both: a kit2 picker field with no seed reads as "unset" and
		// clears the reference on save.
		kit2.FieldSpec{Name: "workflow", Label: "Workflow", Kind: kit2.KPicker, Options: wfOpts, Initial: bulkSetSkip},
		kit2.FieldSpec{Name: "runtime_image", Label: "Runtime image", Kind: kit2.KPicker, Options: imgOpts, Initial: bulkSetSkip},
	)
	m.wireBulkSetForm(f, m.bulkSetIDs, names)
	return f
}

// wireBulkSetForm installs the submit handler: it turns the chosen pair into ONE action and hands
// it to openAction, which raises the confirm that names the count and the value(s).
//
// THE CONFIRM IS RAISED HERE, AFTER THE PICKER, and not in bulkItemActions. A confirm raised before
// the values are known could only name the count; raising it after is what lets it name the values
// too. It is also why the action carries no `Confirm` of its own in the action list — the picker
// has to be opened first.
func (m *Model) wireBulkSetForm(f *kit2.Form, ids []string, wfNames map[string]string) *kit2.Form {
	f.Focused = true
	f.Width = 70
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		wfVal := v["workflow"]
		imgVal := v["runtime_image"]
		if wfVal == bulkSetSkip && imgVal == bulkSetSkip {
			// The form renders this inline and no write happens: a "set" that sets nothing is not a
			// write worth making.
			return nil, errors.New("choose a workflow or a runtime image to set (or to clear)")
		}
		wfLabel := wfVal
		if nm, ok := wfNames[wfVal]; ok {
			wfLabel = nm
		}
		return m.openAction(m.bulkSetAction(ids, wfVal, imgVal, wfLabel)), nil
	}
	return f
}

// bulkSetAction is the counted, sequential write over the marked ids.
func (m *Model) bulkSetAction(ids []string, wfVal, imgVal, wfLabel string) kit2.Action {
	n := len(ids)
	return kit2.Action{
		Label:   "set workflow & image on " + strconv.Itoa(n) + " selected",
		Source:  srcWorkItems,
		Confirm: m.bulkSetConfirmText(ids, wfVal, imgVal, wfLabel),
		Do: func(ctx context.Context) error {
			if m.cl == nil || m.cl.WorkItems == nil {
				return fmt.Errorf("no work-item client")
			}
			// EVERY id is attempted and the failures are COUNTED: a partial bulk operation must say
			// what it did rather than stop at the first rejection and claim a clean sweep (the house
			// style bulk archive/delete already use).
			failed := 0
			reasons := make([]string, 0, 3)
			for _, id := range ids {
				req := &apiv1.UpdateWorkItemRequest{Id: id}
				// ONLY the chosen fields are set. An untouched field stays a nil pointer, i.e. it is
				// OMITTED from the request — which is exactly the proto's `optional` semantic that
				// makes a workflow-only write unable to clobber a runtime image (and vice versa).
				if wfVal != bulkSetSkip {
					wf := wfVal
					req.WorkflowId = &wf
				}
				if imgVal != bulkSetSkip {
					img := imgVal
					req.RuntimeImage = &img
				}
				if _, err := m.cl.WorkItems.UpdateWorkItem(ctx, connect.NewRequest(req)); err != nil {
					failed++
					if len(reasons) < 3 {
						reasons = append(reasons, err.Error())
					}
				}
			}
			if failed > 0 {
				return fmt.Errorf("set %d of %d — %d rejected: %s",
					n-failed, n, failed, strings.Join(reasons, "; "))
			}
			return nil
		},
	}
}

// bulkSetConfirmText names the count and the VALUE(S) being applied, and calls out that a sequence
// parent's own binding is INERT.
//
// The parent note is not decoration: a parent-with-children is a sequence container, so even a
// parent carrying a fresh binding gets a sequence badge and is routed to the sequence engine at
// fire time — its children each run their own workflows. Setting it is still allowed (the
// single-item form permits it too, and refusing here would invent a carve-out the single path does
// not have), but the operator must not believe they ROUTED the parent.
func (m *Model) bulkSetConfirmText(ids []string, wfVal, imgVal, wfLabel string) string {
	var values []string
	switch wfVal {
	case bulkSetSkip:
	case bulkSetClear:
		values = append(values, "workflow: cleared (unbound)")
	default:
		values = append(values, "workflow: "+wfLabel)
	}
	switch imgVal {
	case bulkSetSkip:
	case bulkSetClear:
		values = append(values, "runtime image: cleared (base image)")
	default:
		values = append(values, "runtime image: "+imgVal)
	}
	if len(values) == 0 {
		values = append(values, "nothing")
	}
	n := len(ids)
	items := "items"
	if n == 1 {
		items = "item"
	}
	text := fmt.Sprintf("Set %s on %d %s?\nEach item is written one at a time; a rejected item is counted, not hidden.",
		strings.Join(values, ", "), n, items)
	if parents := m.bulkSetParentCount(ids); parents > 0 {
		verb, noun := "are", "SEQUENCE PARENTS"
		if parents == 1 {
			verb, noun = "is", "SEQUENCE PARENT"
		}
		text += fmt.Sprintf("\nNote: %d of the selection %s a %s — a parent's own workflow/image binding is INERT; its children each run their own workflows. The value is stored anyway and does not route the parent.",
			parents, verb, noun)
	}
	return text
}

// bulkSetParentCount counts the marked ids that have children (a sequence parent). Read under
// viewMu, because parentIDs is written by fetchWorkItems, which runs off the update loop.
func (m *Model) bulkSetParentCount(ids []string) int {
	m.viewMu.Lock()
	defer m.viewMu.Unlock()
	n := 0
	for _, id := range ids {
		if m.parentIDs[id] {
			n++
		}
	}
	return n
}

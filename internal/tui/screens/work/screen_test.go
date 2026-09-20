package work

// screen_test.go — the Work screen is asserted against a REAL Connect client
// talking to fake ProjectService / WorkItemService / RuntimeImageService
// handlers over httptest: the requests the screen actually sends are
// recorded, and the fakes hold the model so "the list reconciles", "the
// order persisted" and "the build status transitioned" are asserted against
// real state, not against the UI.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/protobuf/types/known/timestamppb"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
	"github.com/beardedparrott/orchicon/internal/tui/subs"
)

// ---------------- fake plane ----------------

type fakePlane struct {
	apiv1connect.UnimplementedWorkItemServiceHandler
	apiv1connect.UnimplementedProjectServiceHandler
	// The MCP service is embedded so the fake satisfies the handler interface while
	// implementing only the three calls the project forms make (list, get-selection,
	// set-selection).
	apiv1connect.UnimplementedMCPServiceHandler
	apiv1connect.UnimplementedRuntimeImageServiceHandler
	apiv1connect.UnimplementedWorkflowServiceHandler

	mu        sync.Mutex
	items     map[string]*apiv1.WorkItem
	order     []string
	projects  map[string]*apiv1.Project
	projOrder []string
	// projectMCP is each project's MCP selection, as GetProjectMCPServers would report it.
	projectMCP map[string][]string
	images     map[string]*apiv1.RuntimeImage
	imgOrder   []string
	nextID     int

	created     []*apiv1.CreateWorkItemRequest
	updated     []*apiv1.UpdateWorkItemRequest
	deleted     []string
	archived    []string
	restored    []string
	reorders    []*apiv1.ReorderWorkItemsRequest
	assigned    []*apiv1.AssignWorkerRequest
	unassigned  []string
	projCreated []*apiv1.CreateProjectRequest
	projUpdated []*apiv1.UpdateProjectRequest
	// projActivated records the ids ActivateProject was called with, in order.
	projActivated []string
	projDeleted   []string
	projMCPSet    []*apiv1.ProjectMCPServersSetRequest
	// mcpServers is what ListMCPServers returns; mcpError, when set, makes it fail —
	// which is how a test exercises the "the MCP data did not load" path.
	mcpServers  []*apiv1.MCPServer
	mcpError    error
	dirProbes   []string
	imgCreated  []*apiv1.CreateRuntimeImageRequest
	imgUpdated  []*apiv1.UpdateRuntimeImageRequest
	imgDeleted  []string
	builds      []*apiv1.BuildRuntimeImageRequest
	buildChunks []*apiv1.BuildRuntimeImageResponse

	// workflows / workflowVersions are the bulk-set picker's option source. Empty means the
	// default one-workflow fixture (see ListWorkflows).
	workflows        []*apiv1.Workflow
	workflowVersions map[string]*apiv1.WorkflowVersion
}

func newPlane() *fakePlane {
	return &fakePlane{
		items:      map[string]*apiv1.WorkItem{},
		projects:   map[string]*apiv1.Project{},
		projectMCP: map[string][]string{},
		images:     map[string]*apiv1.RuntimeImage{},
	}
}

func (p *fakePlane) addItem(w *apiv1.WorkItem) *apiv1.WorkItem {
	p.items[w.GetId()] = w
	p.order = append(p.order, w.GetId())
	return w
}

// ---------- WorkItemService ----------

func (p *fakePlane) CreateWorkItem(_ context.Context, req *connect.Request[apiv1.CreateWorkItemRequest]) (*connect.Response[apiv1.CreateWorkItemResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.created = append(p.created, req.Msg)
	p.nextID++
	w := p.addItem(&apiv1.WorkItem{
		Id:                 fmt.Sprintf("wi-new-%d", p.nextID),
		Title:              req.Msg.GetTitle(),
		Kind:               req.Msg.GetKind(),
		Status:             apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING,
		ProjectId:          req.Msg.GetProjectId(),
		ParentId:           req.Msg.GetParentId(),
		Description:        req.Msg.GetDescription(),
		AcceptanceCriteria: req.Msg.GetAcceptanceCriteria(),
		Priority:           req.Msg.GetPriority(),
		Budgets:            req.Msg.GetBudgets(),
		ContextWindow:      req.Msg.GetContextWindow(),
		WorkflowId:         req.Msg.GetWorkflowId(),
		RuntimeImage:       req.Msg.GetRuntimeImage(),
		ContextFiles:       req.Msg.GetContextFiles(),
		AutoStartWorkflow:  boolPtr(req.Msg.GetAutoStartWorkflow()),
		CreatedAt:          timestamppb.Now(),
	})
	return connect.NewResponse(&apiv1.CreateWorkItemResponse{WorkItem: w}), nil
}

func (p *fakePlane) GetWorkItem(_ context.Context, req *connect.Request[apiv1.GetWorkItemRequest]) (*connect.Response[apiv1.GetWorkItemResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w, ok := p.items[req.Msg.GetId()]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("work item not found"))
	}
	return connect.NewResponse(&apiv1.GetWorkItemResponse{WorkItem: w}), nil
}

func (p *fakePlane) ListWorkItems(_ context.Context, req *connect.Request[apiv1.ListWorkItemsRequest]) (*connect.Response[apiv1.ListWorkItemsResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*apiv1.WorkItem
	for _, id := range p.order {
		w := p.items[id]
		if w.GetArchivedAt() != nil != req.Msg.GetIncludeArchived() {
			continue
		}
		if req.Msg.ParentId != nil && w.GetParentId() != req.Msg.GetParentId() {
			continue
		}
		if pid := req.Msg.GetProjectId(); pid != "" && w.GetProjectId() != pid {
			continue
		}
		out = append(out, w)
	}
	return connect.NewResponse(&apiv1.ListWorkItemsResponse{WorkItems: out}), nil
}

func (p *fakePlane) UpdateWorkItem(_ context.Context, req *connect.Request[apiv1.UpdateWorkItemRequest]) (*connect.Response[apiv1.UpdateWorkItemResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w, ok := p.items[req.Msg.GetId()]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("work item not found"))
	}
	p.updated = append(p.updated, req.Msg)
	m := req.Msg
	if m.Title != nil {
		w.Title = m.GetTitle()
	}
	if m.Description != nil {
		w.Description = m.GetDescription()
	}
	if m.AcceptanceCriteria != nil {
		w.AcceptanceCriteria = m.GetAcceptanceCriteria()
	}
	if m.Status != nil {
		w.Status = m.GetStatus()
	}
	if m.Priority != nil {
		w.Priority = m.GetPriority()
	}
	if m.Budgets != nil {
		w.Budgets = m.GetBudgets()
	}
	if m.ContextWindow != nil {
		w.ContextWindow = m.GetContextWindow()
	}
	if m.RuntimeImage != nil {
		w.RuntimeImage = m.GetRuntimeImage()
	}
	if m.WorkflowId != nil {
		w.WorkflowId = m.GetWorkflowId()
	}
	if m.AutoStartWorkflow != nil {
		w.AutoStartWorkflow = boolPtr(m.GetAutoStartWorkflow())
	}
	if m.Kind != nil {
		w.Kind = m.GetKind()
	}
	if m.ContextFiles != nil {
		w.ContextFiles = m.GetContextFiles().GetFiles()
	}
	if m.ScheduledStartAt != nil {
		w.ScheduledStartAt = m.GetScheduledStartAt()
	}
	return connect.NewResponse(&apiv1.UpdateWorkItemResponse{WorkItem: w}), nil
}

// DeleteWorkItem soft-deletes exactly like the plane: status → cancelled.
func (p *fakePlane) DeleteWorkItem(_ context.Context, req *connect.Request[apiv1.DeleteWorkItemRequest]) (*connect.Response[apiv1.DeleteWorkItemResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w, ok := p.items[req.Msg.GetId()]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("work item not found"))
	}
	p.deleted = append(p.deleted, req.Msg.GetId())
	w.Status = apiv1.WorkItemStatus_WORK_ITEM_STATUS_CANCELLED
	return connect.NewResponse(&apiv1.DeleteWorkItemResponse{WorkItem: w}), nil
}

func (p *fakePlane) ArchiveWorkItem(_ context.Context, req *connect.Request[apiv1.ArchiveWorkItemRequest]) (*connect.Response[apiv1.ArchiveWorkItemResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w, ok := p.items[req.Msg.GetId()]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("work item not found"))
	}
	// The real plane blocks archiving while the item has children.
	for _, other := range p.items {
		if other.GetParentId() == req.Msg.GetId() && other.GetArchivedAt() == nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("work item has children"))
		}
	}
	p.archived = append(p.archived, req.Msg.GetId())
	w.ArchivedFromStatus = w.GetStatus().String()
	w.Status = apiv1.WorkItemStatus_WORK_ITEM_STATUS_ARCHIVED
	w.ArchivedAt = timestamppb.Now()
	return connect.NewResponse(&apiv1.ArchiveWorkItemResponse{WorkItem: w}), nil
}

func (p *fakePlane) RestoreWorkItem(_ context.Context, req *connect.Request[apiv1.RestoreWorkItemRequest]) (*connect.Response[apiv1.RestoreWorkItemResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w, ok := p.items[req.Msg.GetId()]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("work item not found"))
	}
	p.restored = append(p.restored, req.Msg.GetId())
	w.Status = statusFromName(strings.ToLower(strings.TrimPrefix(w.GetArchivedFromStatus(), "WORK_ITEM_STATUS_")))
	w.ArchivedAt = nil
	w.ArchivedFromStatus = ""
	return connect.NewResponse(&apiv1.RestoreWorkItemResponse{WorkItem: w}), nil
}

func (p *fakePlane) ReorderWorkItems(_ context.Context, req *connect.Request[apiv1.ReorderWorkItemsRequest]) (*connect.Response[apiv1.ReorderWorkItemsResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reorders = append(p.reorders, req.Msg)
	var out []*apiv1.WorkItem
	for i, id := range req.Msg.GetChildIds() {
		w, ok := p.items[id]
		if !ok {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("work item not found"))
		}
		w.SortOrder = float64(i + 1) // the server-confirmed sequence chain
		out = append(out, w)
	}
	return connect.NewResponse(&apiv1.ReorderWorkItemsResponse{WorkItems: out}), nil
}

func (p *fakePlane) AssignWorker(_ context.Context, req *connect.Request[apiv1.AssignWorkerRequest]) (*connect.Response[apiv1.AssignWorkerResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w, ok := p.items[req.Msg.GetId()]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("work item not found"))
	}
	p.assigned = append(p.assigned, req.Msg)
	w.AssignedWorkerRef = req.Msg.GetWorkerRef()
	w.Status = apiv1.WorkItemStatus_WORK_ITEM_STATUS_ASSIGNED
	return connect.NewResponse(&apiv1.AssignWorkerResponse{WorkItem: w}), nil
}

func (p *fakePlane) UnassignWorker(_ context.Context, req *connect.Request[apiv1.UnassignWorkerRequest]) (*connect.Response[apiv1.UnassignWorkerResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w, ok := p.items[req.Msg.GetId()]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("work item not found"))
	}
	p.unassigned = append(p.unassigned, req.Msg.GetId())
	w.AssignedWorkerRef = ""
	return connect.NewResponse(&apiv1.UnassignWorkerResponse{WorkItem: w}), nil
}

// ---------- ProjectService ----------

func (p *fakePlane) seedProject(id, name string) *apiv1.Project {
	pr := &apiv1.Project{Id: id, Name: name, Slug: strings.ToLower(name), Status: apiv1.ProjectStatus_PROJECT_STATUS_ACTIVE, Version: 1}
	p.projects[id] = pr
	p.projOrder = append(p.projOrder, id)
	return pr
}

func (p *fakePlane) CreateProject(_ context.Context, req *connect.Request[apiv1.CreateProjectRequest]) (*connect.Response[apiv1.CreateProjectResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.projCreated = append(p.projCreated, req.Msg)
	p.nextID++
	pr := p.seedProject(fmt.Sprintf("proj-new-%d", p.nextID), req.Msg.GetName())
	pr.Slug = req.Msg.GetSlug()
	pr.DefaultRuntimeImage = req.Msg.GetDefaultRuntimeImage()
	return connect.NewResponse(&apiv1.CreateProjectResponse{Project: pr}), nil
}

func (p *fakePlane) GetProject(_ context.Context, req *connect.Request[apiv1.GetProjectRequest]) (*connect.Response[apiv1.GetProjectResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pr, ok := p.projects[req.Msg.GetId()]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("project not found"))
	}
	return connect.NewResponse(&apiv1.GetProjectResponse{Project: pr}), nil
}

func (p *fakePlane) ListProjects(_ context.Context, _ *connect.Request[apiv1.ListProjectsRequest]) (*connect.Response[apiv1.ListProjectsResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*apiv1.Project
	for _, id := range p.projOrder {
		out = append(out, p.projects[id])
	}
	return connect.NewResponse(&apiv1.ListProjectsResponse{Projects: out}), nil
}

// DeleteProject mirrors the server's not-found for an unknown id, so a test cannot pass
// by deleting a project that does not exist.
func (p *fakePlane) DeleteProject(_ context.Context, req *connect.Request[apiv1.DeleteProjectRequest]) (*connect.Response[apiv1.DeleteProjectResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.projects[req.Msg.GetId()]; !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("project not found"))
	}
	delete(p.projects, req.Msg.GetId())
	for i, id := range p.projOrder {
		if id == req.Msg.GetId() {
			p.projOrder = append(p.projOrder[:i], p.projOrder[i+1:]...)
			break
		}
	}
	p.projDeleted = append(p.projDeleted, req.Msg.GetId())
	return connect.NewResponse(&apiv1.DeleteProjectResponse{}), nil
}

// ListMCPServers returns the seeded entries, or the seeded error. The ERROR path is the
// point: it is how a test proves that a failed MCP load leaves the form without the field
// rather than with an empty one.
func (p *fakePlane) ListMCPServers(_ context.Context, _ *connect.Request[apiv1.MCPServerListRequest]) (*connect.Response[apiv1.MCPServerListResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mcpError != nil {
		return nil, p.mcpError
	}
	return connect.NewResponse(&apiv1.MCPServerListResponse{Servers: p.mcpServers}), nil
}

func (p *fakePlane) GetProjectMCPServers(_ context.Context, req *connect.Request[apiv1.ProjectMCPServersGetRequest]) (*connect.Response[apiv1.ProjectMCPServersGetResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return connect.NewResponse(&apiv1.ProjectMCPServersGetResponse{McpServerIds: p.projectMCP[req.Msg.GetProjectId()]}), nil
}

// SetProjectMCPServers records the write AND updates the stored selection, so a read-back
// sees what a save produced. It does not validate the ids: the server treats them as
// references, and this fake's job is to record what the TUI sent.
func (p *fakePlane) SetProjectMCPServers(_ context.Context, req *connect.Request[apiv1.ProjectMCPServersSetRequest]) (*connect.Response[apiv1.ProjectMCPServersSetResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.projMCPSet = append(p.projMCPSet, req.Msg)
	if p.projectMCP == nil {
		p.projectMCP = map[string][]string{}
	}
	p.projectMCP[req.Msg.GetProjectId()] = req.Msg.GetMcpServerIds()
	return connect.NewResponse(&apiv1.ProjectMCPServersSetResponse{McpServerIds: req.Msg.GetMcpServerIds()}), nil
}

// ActivateProject mirrors the SERVER'S PRECONDITION rather than accepting anything:
// the real UPDATE carries `AND status = 'drafting'`, so activating a non-drafting
// project fails there. A fake that always succeeded would let a test prove the action
// is wired while hiding that it is offered in states where it cannot work.
//
// SEEDED PROJECTS ARE ACTIVE (seedProject), so a test that wants the drafting path
// must set the status itself — which is the honest fixture, because the real server
// creates projects drafting and this fake does not model CreateProject's status.
func (p *fakePlane) ActivateProject(_ context.Context, req *connect.Request[apiv1.ActivateProjectRequest]) (*connect.Response[apiv1.ActivateProjectResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pr, ok := p.projects[req.Msg.GetId()]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("project not found"))
	}
	if pr.GetStatus() != apiv1.ProjectStatus_PROJECT_STATUS_DRAFTING {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("project is %q, must be drafting", strings.ToLower(pr.GetStatus().String())))
	}
	pr.Status = apiv1.ProjectStatus_PROJECT_STATUS_ACTIVE
	p.projActivated = append(p.projActivated, pr.GetId())
	return connect.NewResponse(&apiv1.ActivateProjectResponse{Project: pr}), nil
}

func (p *fakePlane) UpdateProject(_ context.Context, req *connect.Request[apiv1.UpdateProjectRequest]) (*connect.Response[apiv1.UpdateProjectResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pr, ok := p.projects[req.Msg.GetId()]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("project not found"))
	}
	p.projUpdated = append(p.projUpdated, req.Msg)
	m := req.Msg
	if m.Name != nil {
		pr.Name = m.GetName()
	}
	if m.ProjectDir != nil {
		pr.ProjectDir = m.GetProjectDir()
	}
	if m.Goals != nil {
		var parts []string
		for _, g := range m.GetGoals().GetFields() {
			parts = append(parts, g.GetKey()+"="+g.GetValue())
		}
		pr.Goals = strings.Join(parts, ",")
	}
	if m.DefaultRuntimeImage != nil {
		pr.DefaultRuntimeImage = m.GetDefaultRuntimeImage()
	}
	pr.Version++
	return connect.NewResponse(&apiv1.UpdateProjectResponse{Project: pr}), nil
}

// ListProjectFiles is the directory probe: it fails for an unset directory,
// exactly the signal that "the project directory exists".
func (p *fakePlane) ListProjectFiles(_ context.Context, req *connect.Request[apiv1.ListProjectFilesRequest]) (*connect.Response[apiv1.ListProjectFilesResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pr, ok := p.projects[req.Msg.GetId()]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("project not found"))
	}
	p.dirProbes = append(p.dirProbes, req.Msg.GetId())
	if pr.GetProjectDir() == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("project_dir is not set"))
	}
	return connect.NewResponse(&apiv1.ListProjectFilesResponse{
		ParentPath: pr.GetProjectDir(),
		DirName:    "orchicon",
		Entries:    []*apiv1.FileTreeEntry{{Name: "go.mod", Path: pr.GetProjectDir() + "/go.mod"}},
	}), nil
}

// ---------- WorkflowService ----------

func (p *fakePlane) ListWorkflows(context.Context, *connect.Request[apiv1.ListWorkflowsRequest]) (*connect.Response[apiv1.ListWorkflowsResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// The default fixture is the one the pre-existing tests expect; a test that seeds
	// `workflows` gets its own list (so a runnable/stepless/deprecated mix can be exercised).
	if len(p.workflows) == 0 {
		return connect.NewResponse(&apiv1.ListWorkflowsResponse{Workflows: []*apiv1.Workflow{{Id: "wf-1", Name: "Fanout"}}}), nil
	}
	return connect.NewResponse(&apiv1.ListWorkflowsResponse{Workflows: p.workflows}), nil
}

// GetWorkflow serves each seeded workflow's latest version, which is the only place the STEPS live
// (the Workflow message carries none) — so this is what the bulk-set picker's runnable filter reads.
func (p *fakePlane) GetWorkflow(_ context.Context, req *connect.Request[apiv1.GetWorkflowRequest]) (*connect.Response[apiv1.GetWorkflowResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, w := range p.workflows {
		if w.GetId() != req.Msg.GetId() {
			continue
		}
		return connect.NewResponse(&apiv1.GetWorkflowResponse{
			Workflow:      w,
			LatestVersion: p.workflowVersions[w.GetId()],
		}), nil
	}
	return nil, connect.NewError(connect.CodeNotFound, errors.New("workflow not found"))
}

// ---------- RuntimeImageService ----------

func (p *fakePlane) seedImage(id, name string, status apiv1.RuntimeImageStatus) *apiv1.RuntimeImage {
	img := &apiv1.RuntimeImage{Id: id, Name: name, Slug: name, Tag: name + ":latest", Version: 3, Status: status}
	p.images[id] = img
	p.imgOrder = append(p.imgOrder, id)
	return img
}

func (p *fakePlane) CreateRuntimeImage(_ context.Context, req *connect.Request[apiv1.CreateRuntimeImageRequest]) (*connect.Response[apiv1.CreateRuntimeImageResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.imgCreated = append(p.imgCreated, req.Msg)
	p.nextID++
	img := p.seedImage(fmt.Sprintf("img-new-%d", p.nextID), req.Msg.GetName(), apiv1.RuntimeImageStatus_RUNTIME_IMAGE_STATUS_DRAFT)
	img.Slug = req.Msg.GetSlug()
	img.Description = req.Msg.GetDescription()
	img.AptPackages = req.Msg.GetAptPackages()
	img.Toolchains = req.Msg.GetToolchains()
	img.Env = req.Msg.GetEnv()
	img.DockerfileOverride = req.Msg.GetDockerfileOverride()
	img.Tag = req.Msg.GetTag()
	return connect.NewResponse(&apiv1.CreateRuntimeImageResponse{RuntimeImage: img}), nil
}

func (p *fakePlane) GetRuntimeImage(_ context.Context, req *connect.Request[apiv1.GetRuntimeImageRequest]) (*connect.Response[apiv1.GetRuntimeImageResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	img, ok := p.images[req.Msg.GetId()]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("image not found"))
	}
	return connect.NewResponse(&apiv1.GetRuntimeImageResponse{RuntimeImage: img}), nil
}

func (p *fakePlane) ListRuntimeImages(context.Context, *connect.Request[apiv1.ListRuntimeImagesRequest]) (*connect.Response[apiv1.ListRuntimeImagesResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*apiv1.RuntimeImage
	for _, id := range p.imgOrder {
		out = append(out, p.images[id])
	}
	return connect.NewResponse(&apiv1.ListRuntimeImagesResponse{RuntimeImages: out}), nil
}

func (p *fakePlane) UpdateRuntimeImage(_ context.Context, req *connect.Request[apiv1.UpdateRuntimeImageRequest]) (*connect.Response[apiv1.UpdateRuntimeImageResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	img, ok := p.images[req.Msg.GetId()]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("image not found"))
	}
	p.imgUpdated = append(p.imgUpdated, req.Msg)
	m := req.Msg
	if m.Name != nil {
		img.Name = m.GetName()
	}
	if m.AptPackages != nil {
		img.AptPackages = m.GetAptPackages()
	}
	if m.Toolchains != nil {
		img.Toolchains = m.GetToolchains()
	}
	if m.Env != nil {
		img.Env = m.GetEnv()
	}
	if m.DockerfileOverride != nil {
		img.DockerfileOverride = m.GetDockerfileOverride()
	}
	if m.Tag != nil {
		img.Tag = m.GetTag()
	}
	img.Version++
	return connect.NewResponse(&apiv1.UpdateRuntimeImageResponse{RuntimeImage: img}), nil
}

func (p *fakePlane) DeleteRuntimeImage(_ context.Context, req *connect.Request[apiv1.DeleteRuntimeImageRequest]) (*connect.Response[apiv1.DeleteRuntimeImageResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.images[req.Msg.GetId()]; !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("image not found"))
	}
	p.imgDeleted = append(p.imgDeleted, req.Msg.GetId())
	delete(p.images, req.Msg.GetId())
	return connect.NewResponse(&apiv1.DeleteRuntimeImageResponse{}), nil
}

// BuildRuntimeImage streams log chunks and a terminal status, exactly the
// daemon's contract (chunk per line, final message carries the status).
func (p *fakePlane) BuildRuntimeImage(_ context.Context, req *connect.Request[apiv1.BuildRuntimeImageRequest], stream *connect.ServerStream[apiv1.BuildRuntimeImageResponse]) error {
	p.mu.Lock()
	p.builds = append(p.builds, req.Msg)
	chunks := append([]*apiv1.BuildRuntimeImageResponse{}, p.buildChunks...)
	img, ok := p.images[req.Msg.GetId()]
	p.mu.Unlock()
	if !ok {
		return connect.NewError(connect.CodeNotFound, errors.New("image not found"))
	}
	for _, c := range chunks {
		if err := stream.Send(c); err != nil {
			return err
		}
	}
	if img.GetStatus() == apiv1.RuntimeImageStatus_RUNTIME_IMAGE_STATUS_READY {
		return stream.Send(&apiv1.BuildRuntimeImageResponse{Log: "build skipped\n", Status: apiv1.RuntimeImageStatus_RUNTIME_IMAGE_STATUS_READY, Skipped: true, Tag: img.GetTag()})
	}
	p.mu.Lock()
	img.Status = apiv1.RuntimeImageStatus_RUNTIME_IMAGE_STATUS_READY
	img.BuiltVersion = img.GetVersion()
	p.mu.Unlock()
	return stream.Send(&apiv1.BuildRuntimeImageResponse{Log: "build done\n", Status: apiv1.RuntimeImageStatus_RUNTIME_IMAGE_STATUS_READY, Tag: img.GetTag()})
}

// ---------------- harness ----------------

func newModel(t *testing.T, p *fakePlane) *Model {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(apiv1connect.NewWorkItemServiceHandler(p))
	mux.Handle(apiv1connect.NewProjectServiceHandler(p))
	mux.Handle(apiv1connect.NewMCPServiceHandler(p))
	mux.Handle(apiv1connect.NewRuntimeImageServiceHandler(p))
	mux.Handle(apiv1connect.NewWorkflowServiceHandler(p))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	m := New(client.New(client.Options{BaseURL: srv.URL}), subs.NewRegistry(), "")
	m.SetSize(400, 40)
	return m
}

func kmsg(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "space":
		return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")}
	case "ctrl+x":
		// A REAL control key, not the literal runes "ctrl+x". The terminal sends one
		// control byte (CAN, 0x18) and bubbletea reports KeyCtrlX; KeyMsg.String() then
		// returns "ctrl+x", which is what the router matches on. The literal-runes form
		// would ALSO match — String() just returns the runes — so this is about fidelity
		// rather than necessity: the test should drive the key the terminal actually
		// sends, since a helper that quietly accepts a shape the real input never takes
		// can pass while the binding is broken.
		return tea.KeyMsg{Type: tea.KeyCtrlX}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// press drives one key through the screen and returns the cmd it produced.
func press(t *testing.T, m *Model, key string) tea.Cmd {
	t.Helper()
	_, cmd := m.Update(kmsg(key))
	return cmd
}

// submit focuses the form's last field and presses enter (kit2's typed-form
// submit gesture; validation runs first).
func submit(t *testing.T, m *Model, lastField string) tea.Cmd {
	t.Helper()
	f := m.ActiveForm()
	if f == nil {
		t.Fatal("no open form to submit")
	}
	if !f.FocusName(lastField) {
		t.Fatalf("form has no field %q", lastField)
	}
	return press(t, m, "ctrl+s")
}

// run executes a cmd and feeds its message back into the screen.
func run(t *testing.T, m *Model, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a command, got nil")
	}
	msg := cmd()
	if msg == nil {
		t.Fatal("command produced no message")
	}
	m.Update(msg)
}

// load drives one source's fetch through the real fetch func and feeds the
// result into the screen (the shell's load path).
func load(t *testing.T, m *Model, src string) {
	t.Helper()
	for _, s := range m.Base.SourcesForTest() {
		if s.Name != src {
			continue
		}
		items, next, err := s.Fetch(context.Background(), "")
		if err != nil {
			t.Fatalf("fetch %s: %v", src, err)
		}
		if !m.Base.LoadItems(src, items, next) {
			t.Fatalf("no source %q to load", src)
		}
		return
	}
	t.Fatalf("no source %q registered", src)
}

func itemsOf(m *Model, src string) []screenkit.Item {
	for _, s := range m.Base.SourcesForTest() {
		if s.Name == src {
			return s.Items
		}
	}
	return nil
}

func titles(items []screenkit.Item) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Title)
	}
	return out
}

func hasTitle(items []screenkit.Item, want string) bool {
	for _, it := range items {
		if it.Title == want {
			return true
		}
	}
	return false
}

// id0 returns the first project id the fake holds (test convenience).
func id0(p *fakePlane) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.projOrder[0]
}

func metaOf(items []screenkit.Item, id string) string {
	for _, it := range items {
		if it.ID == id {
			return it.Meta
		}
	}
	return ""
}

// ------------- work items: create / edit / status -------------

func TestWorkItemCreateFromForm(t *testing.T) {
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	p.addItem(&apiv1.WorkItem{Id: "wi-epic", Title: "Epic", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC, Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, ProjectId: "proj-1"})
	m := newModel(t, p)
	m.SelectSource(srcWorkItems)

	// 'n' loads the option lists (projects + workflows) before the form.
	run(t, m, press(t, m, "n"))
	f := m.ActiveForm()
	if f == nil {
		t.Fatal("n must open the create form")
	}
	if !m.ClaimsKeys() {
		t.Fatal("an open form must claim the keys (typed characters are never shell shortcuts)")
	}

	f.FocusName("title")
	for _, ch := range "Retry the sweeper" {
		if ch == ' ' {
			press(t, m, "space")
			continue
		}
		press(t, m, string(ch))
	}
	if got := f.Values["title"]; got != "Retry the sweeper" {
		t.Fatalf("typed title = %q", got)
	}
	f.Set("project", "proj-1")
	f.Set("kind", "subtask")
	f.Set("parent", "wi-epic")
	f.Set("priority", "3")
	f.Set("budgets", `{"tokens":100000}`)
	f.Set("context_window", "16000")
	f.Set("auto_start", "true")

	run(t, m, submit(t, m, "auto_start"))

	if m.ActiveForm() != nil {
		t.Fatal("the form must close after a successful create")
	}
	if len(p.created) != 1 {
		t.Fatalf("CreateWorkItem calls = %d, want 1", len(p.created))
	}
	req := p.created[0]
	if req.GetTitle() != "Retry the sweeper" || req.GetParentId() != "wi-epic" || req.GetKind() != apiv1.WorkItemKind_WORK_ITEM_KIND_SUBTASK {
		t.Fatalf("create request = %+v", req)
	}
	if req.GetProjectId() != "proj-1" || req.GetPriority() != 3 || req.GetContextWindow() != 16000 ||
		req.GetBudgets() != `{"tokens":100000}` || !req.GetAutoStartWorkflow() {
		t.Fatalf("create request lost mutable fields: %+v", req)
	}
	// The new item reconciles into the tree, nested one level under its parent.
	// The nesting is the row's DEPTH now (the pane draws the indent), not
	// whitespace baked into the title.
	load(t, m, srcWorkItems)
	rows := itemsOf(m, srcWorkItems)
	depth := -1
	for _, r := range rows {
		if strings.HasSuffix(r.Title, "Retry the sweeper") {
			depth = r.Depth
		}
	}
	if depth != 1 {
		t.Fatalf("created item must nest one level under its parent: depth=%d rows=%v", depth, titles(rows))
	}
}

func TestWorkItemEditEveryMutableField(t *testing.T) {
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	p.addItem(&apiv1.WorkItem{
		Id: "wi-1", Title: "Old title", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK, ProjectId: "proj-1",
		Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, Description: "old", AcceptanceCriteria: "old ac",
		Priority: 1, Version: 2, CreatedAt: timestamppb.Now(),
	})
	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)

	run(t, m, press(t, m, "e"))
	f := m.ActiveForm()
	if f == nil {
		t.Fatal("e must open the edit form")
	}
	if f.Values["title"] != "Old title" || f.Values["description"] != "old" || f.Values["acceptance"] != "old ac" {
		t.Fatalf("the edit form must prefill from real fields: %+v", f.Values)
	}
	f.Set("title", "New title")
	f.Set("description", "new body")
	f.Set("acceptance", "new ac")
	f.Set("status", "ready")
	f.Set("priority", "7")
	f.Set("budgets", `{"usd":5}`)
	f.Set("context_window", "32000")
	f.Set("runtime_image", "orchicon-runtime-go:latest")
	f.Set("context_files", "/tmp/a.go,/tmp/b")
	f.Set("scheduled_start", "2026-09-01T09:00:00Z")
	f.Set("auto_start", "true")

	run(t, m, submit(t, m, "auto_start"))

	if len(p.updated) != 1 {
		t.Fatalf("UpdateWorkItem calls = %d", len(p.updated))
	}
	req := p.updated[0]
	if req.GetId() != "wi-1" || req.GetTitle() != "New title" || req.GetDescription() != "new body" ||
		req.GetAcceptanceCriteria() != "new ac" || req.GetStatus() != apiv1.WorkItemStatus_WORK_ITEM_STATUS_READY ||
		req.GetPriority() != 7 || req.GetBudgets() != `{"usd":5}` || req.GetContextWindow() != 32000 ||
		req.GetRuntimeImage() != "orchicon-runtime-go:latest" || !req.GetAutoStartWorkflow() {
		t.Fatalf("edit request lost mutable fields: %+v", req)
	}
	if strings.Join(req.GetContextFiles().GetFiles(), ",") != "/tmp/a.go,/tmp/b" {
		t.Fatalf("context files = %v", req.GetContextFiles().GetFiles())
	}
	if req.GetScheduledStartAt() == nil {
		t.Fatal("the schedule must round-trip through the edit form")
	}
	// Local reconcile: the item's real status is now ready.
	load(t, m, srcWorkItems)
	if got := metaOf(itemsOf(m, srcWorkItems), "wi-1"); !strings.HasPrefix(got, "ready") {
		t.Fatalf("row meta after edit = %q, want the ready pill", got)
	}
}

func TestWorkItemChangeStatusAndPriority(t *testing.T) {
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	p.addItem(&apiv1.WorkItem{Id: "wi-1", Title: "Item", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK, ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING})
	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)

	run(t, m, press(t, m, "s"))
	f := m.ActiveForm()
	if f == nil {
		t.Fatal("s must open the status/priority form")
	}
	f.Set("status", "failed")
	f.Set("priority", "9")
	run(t, m, submit(t, m, "priority"))

	req := p.updated[len(p.updated)-1]
	if req.GetStatus() != apiv1.WorkItemStatus_WORK_ITEM_STATUS_FAILED || req.GetPriority() != 9 {
		t.Fatalf("status request = %+v", req)
	}
	if req.Title != nil {
		t.Fatal("the quick status change must not resend unrelated fields")
	}
}

// ------------- Tree / Board / Archive -------------

// seedHierarchy builds a real Epic → Feature → Task → Subtask chain.
func seedHierarchy(p *fakePlane) {
	p.seedProject("proj-1", "Orchicon")
	p.addItem(&apiv1.WorkItem{Id: "wi-epic", Title: "Epic E", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC, ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, SortOrder: 1})
	p.addItem(&apiv1.WorkItem{Id: "wi-feat", Title: "Feature F", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_FEATURE, ProjectId: "proj-1", ParentId: "wi-epic", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, SortOrder: 1})
	p.addItem(&apiv1.WorkItem{Id: "wi-task", Title: "Task T", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK, ProjectId: "proj-1", ParentId: "wi-feat", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_RUNNING, SortOrder: 1})
	p.addItem(&apiv1.WorkItem{Id: "wi-sub", Title: "Subtask S", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_SUBTASK, ProjectId: "proj-1", ParentId: "wi-task", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_SUCCEEDED, SortOrder: 1})
}

func TestTreeViewRendersRealHierarchy(t *testing.T) {
	p := newPlane()
	seedHierarchy(p)
	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)

	got := titles(itemsOf(m, srcWorkItems))
	// The titles are FLUSH (no baked-in indent): the hierarchy now travels on
	// the row itself, and the list pane draws the indent so it cannot be
	// applied twice.
	want := []string{
		"[epic] Epic E",
		"[feature] Feature F",
		"[task] Task T",
		"[subtask] Subtask S",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("tree rows = %v\nwant %v", got, want)
	}
	// The REAL hierarchy is the row's Depth (epic → feature → task → subtask).
	rows := itemsOf(m, srcWorkItems)
	wantDepths := []int{0, 1, 2, 3}
	if len(rows) != len(wantDepths) {
		t.Fatalf("tree rows = %d, want %d", len(rows), len(wantDepths))
	}
	for i, r := range rows {
		if r.Depth != wantDepths[i] {
			t.Fatalf("row %d (%q) depth = %d, want %d", i, r.Title, r.Depth, wantDepths[i])
		}
	}
	// Only parents carry a collapse toggle; the epic is a root parent and the
	// subtask is the leaf.
	if rows[0].Parent != "" || !rows[0].HasChildren {
		t.Fatalf("the epic must be a root parent: %+v", rows[0])
	}
	if rows[3].HasChildren {
		t.Fatalf("the subtask is a leaf and must not draw a toggle: %+v", rows[3])
	}
	// Kind badges + state pills come from the real fields.
	if meta := metaOf(itemsOf(m, srcWorkItems), "wi-task"); !strings.HasPrefix(meta, "running") {
		t.Fatalf("state pill = %q, want running", meta)
	}
	view := m.View()
	for _, want := range []string{"[epic]", "[feature]", "[task]", "[subtask]", "running", "succeeded"} {
		if !strings.Contains(view, want) {
			t.Errorf("tree view missing %q", want)
		}
	}
}

// The Board view was REMOVED: a status-grouped Kanban does not read as a list
// in a single-column terminal pane. The cycle is now tree -> archive, and the
// retired 'B' chord must do nothing at all.
func TestBoardViewIsGone(t *testing.T) {
	p := newPlane()
	seedHierarchy(p)
	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)

	if m.ViewMode() != viewTree {
		t.Fatalf("the view opens as tree, got %q", m.ViewMode())
	}
	// 'B' is retired: no view change, and the tree still renders.
	press(t, m, "B")
	if m.ViewMode() != viewTree {
		t.Fatalf("'B' must no longer switch views, got %q", m.ViewMode())
	}
	rows := itemsOf(m, srcWorkItems)
	if hasTitle(rows, "── pending (2)") {
		t.Fatalf("a status column must not render: %v", titles(rows))
	}
	if !hasTitle(rows, "[epic] Epic E") {
		t.Fatalf("the tree must still render: %v", titles(rows))
	}

	// 'v' cycles tree -> archive -> tree.
	press(t, m, "v")
	if m.ViewMode() != viewArchive {
		t.Fatalf("v must cycle to archive, got %q", m.ViewMode())
	}
	press(t, m, "v")
	if m.ViewMode() != viewTree {
		t.Fatalf("v must cycle back to tree, got %q", m.ViewMode())
	}

	// Display switch never mutates or writes.
	if len(p.reorders) != 0 {
		t.Fatalf("switching views must not call ReorderWorkItems: %+v", p.reorders)
	}
	if len(p.updated) != 0 {
		t.Fatalf("switching views must not write: %+v", p.updated)
	}
}

func TestArchiveViewListsArchivedItems(t *testing.T) {
	p := newPlane()
	seedHierarchy(p)
	// A terminal childless item, then archive it (children block archiving).
	p.addItem(&apiv1.WorkItem{Id: "wi-done", Title: "Done thing", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK, ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_SUCCEEDED})
	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)

	// Select the archived candidate (last row of the tree) and archive it.
	for i := 0; i < len(itemsOf(m, srcWorkItems))-1; i++ {
		press(t, m, "down")
	}
	press(t, m, "a")
	if !m.DialogOpen() {
		t.Fatal("archive must be confirmed — it is consequential")
	}
	if len(p.archived) != 0 {
		t.Fatal("no archive may be sent before confirmation")
	}
	run(t, m, press(t, m, "enter"))
	if len(p.archived) != 1 || p.archived[0] != "wi-done" {
		t.Fatalf("archived = %v", p.archived)
	}

	// It left the active tree…
	load(t, m, srcWorkItems)
	for _, it := range itemsOf(m, srcWorkItems) {
		if it.ID == "wi-done" {
			t.Fatal("an archived item must leave the active views")
		}
	}
	// …and the Archive view lists it with the status it restores to.
	press(t, m, "Z")
	load(t, m, srcWorkItems)
	rows := itemsOf(m, srcWorkItems)
	if !hasTitle(rows, "[task] Done thing") {
		t.Fatalf("archive view rows = %v", titles(rows))
	}
	if meta := metaOf(rows, "wi-done"); !strings.Contains(meta, "archived") || !strings.Contains(meta, "WORK_ITEM_STATUS_SUCCEEDED") {
		t.Fatalf("archive row must name the restore status, meta = %q", meta)
	}
}

// ------------- reorder (sequence) -------------

func TestReorderChildrenPersists(t *testing.T) {
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	p.addItem(&apiv1.WorkItem{Id: "wi-epic", Title: "Epic", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC, ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING})
	p.addItem(&apiv1.WorkItem{Id: "wi-a", Title: "A", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK, ParentId: "wi-epic", ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, SortOrder: 1})
	p.addItem(&apiv1.WorkItem{Id: "wi-b", Title: "B", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK, ParentId: "wi-epic", ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, SortOrder: 2})
	p.addItem(&apiv1.WorkItem{Id: "wi-c", Title: "C", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK, ParentId: "wi-epic", ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, SortOrder: 3})
	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)

	press(t, m, "down")         // select wi-a (the first step)
	run(t, m, press(t, m, "-")) // move it DOWN one step

	if len(p.reorders) != 1 {
		t.Fatalf("ReorderWorkItems calls = %d, want 1", len(p.reorders))
	}
	req := p.reorders[0]
	if req.GetProjectId() != "proj-1" || req.GetParentId() != "wi-epic" {
		t.Fatalf("reorder request scope = %+v", req)
	}
	if strings.Join(req.GetChildIds(), ",") != "wi-b,wi-a,wi-c" {
		t.Fatalf("reorder child_ids = %v, want wi-b,wi-a,wi-c", req.GetChildIds())
	}
	// The new sequence persisted: the tree now renders it IN THE NEW ORDER, and
	// each step is NUMBERED by its position in the stored sequence. Titles are
	// flush and the nesting is the row's Depth, so every child of the epic sits
	// at depth 1. The epic itself is unnumbered because it is an only child at
	// the top level (a group of one has no order to show).
	load(t, m, srcWorkItems)
	rows := itemsOf(m, srcWorkItems)
	got := titles(rows)
	want := []string{"[epic] Epic", "1. [task] B", "2. [task] A", "3. [task] C"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("tree after reorder = %v\nwant %v", got, want)
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].Depth != 1 {
			t.Fatalf("row %d (%q) must nest one level under the epic: depth=%d", i, rows[i].Title, rows[i].Depth)
		}
	}
	if _, ok := p.items["wi-c"]; !ok {
		t.Fatal("reorder must touch only the listed siblings")
	}
}

// ------------- destructive actions -------------

func TestDeleteRequiresConfirmThenReconciles(t *testing.T) {
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	p.addItem(&apiv1.WorkItem{Id: "wi-1", Title: "Item", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK, ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING})
	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)

	// THE SHARED DELETE CHORD, read from the constant rather than spelled out: `ctrl+x` is
	// what the client's other panes answer and what the operator asked for across the board.
	press(t, m, kit2.DeleteChord)
	if !m.DialogOpen() {
		t.Fatal("delete must open a Confirm dialog")
	}
	if len(p.deleted) != 0 {
		t.Fatal("no delete may be sent before confirmation")
	}
	press(t, m, "esc")
	if m.DialogOpen() || len(p.deleted) != 0 {
		t.Fatal("a dismissed Confirm must not delete")
	}

	press(t, m, "x")
	run(t, m, press(t, m, "enter"))
	if len(p.deleted) != 1 || p.deleted[0] != "wi-1" {
		t.Fatalf("deleted = %v", p.deleted)
	}
	// DeleteWorkItem soft-deletes: the item reconciles as cancelled.
	p.mu.Lock()
	st := p.items["wi-1"].GetStatus()
	p.mu.Unlock()
	if st != apiv1.WorkItemStatus_WORK_ITEM_STATUS_CANCELLED {
		t.Fatalf("delete must soft-delete to cancelled, got %v", st)
	}
	load(t, m, srcWorkItems)
	if meta := metaOf(itemsOf(m, srcWorkItems), "wi-1"); !strings.HasPrefix(meta, "cancelled") {
		t.Fatalf("the list must reconcile after the delete, meta = %q", meta)
	}
}

func TestArchiveRestoreFromArchiveView(t *testing.T) {
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	p.addItem(&apiv1.WorkItem{Id: "wi-1", Title: "Item", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK, ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_SUCCEEDED})
	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)

	press(t, m, "a")
	run(t, m, press(t, m, "enter"))
	if len(p.archived) != 1 {
		t.Fatalf("ArchiveWorkItem calls = %v", p.archived)
	}

	press(t, m, "Z")
	load(t, m, srcWorkItems)
	if !hasTitle(itemsOf(m, srcWorkItems), "[task] Item") {
		t.Fatalf("the archive view must list the item: %v", titles(itemsOf(m, srcWorkItems)))
	}
	press(t, m, "R")
	if !m.DialogOpen() {
		t.Fatal("restore must be confirmed")
	}
	run(t, m, press(t, m, "enter"))
	if len(p.restored) != 1 || p.restored[0] != "wi-1" {
		t.Fatalf("restored = %v", p.restored)
	}
	p.mu.Lock()
	back := p.items["wi-1"].GetStatus()
	p.mu.Unlock()
	if back != apiv1.WorkItemStatus_WORK_ITEM_STATUS_SUCCEEDED {
		t.Fatalf("restore returns the item to its archived_from_status, got %v", back)
	}
	// The archive view reconciles (the item left it).
	load(t, m, srcWorkItems)
	if len(itemsOf(m, srcWorkItems)) != 0 {
		t.Fatalf("restored item must leave the archive view: %v", titles(itemsOf(m, srcWorkItems)))
	}
}

func TestScheduleWorkItem(t *testing.T) {
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	p.addItem(&apiv1.WorkItem{Id: "wi-1", Title: "Item", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK, ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING})
	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)

	// Scheduling lives in the DETAILS pane now: 't' is gone and 'e' is the one
	// way in, carrying the Scheduled-start picker with it.
	m2 := newModel(t, p)
	m2.SelectSource(srcWorkItems)
	load(t, m2, srcWorkItems)
	press(t, m2, "t")
	if m2.form != nil || m2.Base.EditingDetail() {
		t.Fatal("'t' must no longer open a schedule form")
	}

	run(t, m, press(t, m, "e"))
	f := m.ActiveForm()
	if f == nil {
		t.Fatal("e must open the details editor")
	}
	if !f.FocusName("scheduled_start") {
		t.Fatal("the editor must carry a scheduled-start field")
	}
	f.Set("scheduled_start", "2026-09-01T09:00:00Z")
	f.Set("auto_start", "true")
	run(t, m, press(t, m, "ctrl+s"))

	if len(p.updated) == 0 {
		t.Fatal("saving the editor must send an update")
	}
	req := p.updated[len(p.updated)-1]
	if req.GetScheduledStartAt() == nil || !req.GetAutoStartWorkflow() {
		t.Fatalf("schedule request = %+v", req)
	}
	if got := req.GetScheduledStartAt().AsTime().UTC().Format("2006-01-02T15:04:05Z"); got != "2026-09-01T09:00:00Z" {
		t.Fatalf("scheduled start = %s", got)
	}
}

// ------------- projects -------------

func TestProjectCreateEditAndDirectory(t *testing.T) {
	p := newPlane()
	m := newModel(t, p)
	m.SelectSource(srcProjects)

	// create
	// The create form is opened through the PREP now (the MCP options are a round trip),
	// so the returned command must be driven before the form exists — same as every other
	// async form in this suite.
	run(t, m, press(t, m, "n"))
	f := m.ActiveForm()
	if f == nil {
		t.Fatal("n on Projects must open the create form")
	}
	f.Set("name", "Orchicon")
	f.Set("slug", "orchicon")
	f.Set("goals", "ship=parity, quality=high")
	run(t, m, submit(t, m, "default_runtime_image"))

	if len(p.projCreated) != 1 {
		t.Fatalf("CreateProject calls = %d", len(p.projCreated))
	}
	req := p.projCreated[0]
	if req.GetName() != "Orchicon" || req.GetSlug() != "orchicon" || len(req.GetGoals()) != 2 ||
		req.GetGoals()[0].GetKey() != "ship" || req.GetGoals()[0].GetValue() != "parity" {
		t.Fatalf("create project request = %+v goals=%+v", req, req.GetGoals())
	}
	load(t, m, srcProjects)
	if !hasTitle(itemsOf(m, srcProjects), "Orchicon") {
		t.Fatalf("created project must render: %v", titles(itemsOf(m, srcProjects)))
	}

	// edit title / goals / project_dir
	run(t, m, press(t, m, "e"))
	f = m.ActiveForm()
	if f == nil {
		t.Fatal("e must open the project edit form")
	}
	if f.Values["name"] != "Orchicon" {
		t.Fatalf("edit form must prefill, got %+v", f.Values)
	}
	f.Set("name", "Orchicon 2")
	f.Set("goals", "ship=parity")
	f.Set("project_dir", "/home/me/projects/orchicon")
	run(t, m, submit(t, m, "default_runtime_image"))

	up := p.projUpdated[len(p.projUpdated)-1]
	if up.GetName() != "Orchicon 2" || up.GetProjectDir() != "/home/me/projects/orchicon" ||
		len(up.GetGoals().GetFields()) != 1 || up.GetGoals().GetFields()[0].GetValue() != "parity" {
		t.Fatalf("update project request = %+v", up)
	}
	// A project render reconciles the edit.
	load(t, m, srcProjects)
	if p.projects[id0(p)].GetName() != "Orchicon 2" {
		t.Fatalf("the project must reconcile after the edit")
	}

	// set/create the directory explicitly (the d action)
	p.mu.Lock()
	id := p.projOrder[0]
	p.projects[id].ProjectDir = ""
	p.mu.Unlock()
	run(t, m, press(t, m, "d"))
	f = m.ActiveForm()
	if f == nil {
		t.Fatal("d must open the project directory form")
	}
	f.Set("project_dir", "/home/me/projects/orchicon")
	run(t, m, submit(t, m, "project_dir"))
	last := p.projUpdated[len(p.projUpdated)-1]
	if last.GetProjectDir() != "/home/me/projects/orchicon" {
		t.Fatalf("set-directory request = %+v", last)
	}
	if len(p.dirProbes) < 1 {
		t.Fatal("setting the directory must probe it (UpdateProject + ListProjectFiles)")
	}
}

// ------------- runtime images -------------

func TestRuntimeImageCreateEditDelete(t *testing.T) {
	p := newPlane()
	p.seedImage("img-1", "existing", apiv1.RuntimeImageStatus_RUNTIME_IMAGE_STATUS_DRAFT)
	m := newModel(t, p)
	m.SelectSource(srcImages)
	load(t, m, srcImages)

	// create
	press(t, m, "n")
	f := m.ActiveForm()
	if f == nil {
		t.Fatal("n on Runtime Images must open the create form")
	}
	f.Set("name", "Go + Node")
	f.Set("slug", "orchicon-runtime-go-node")
	f.Set("apt_packages", `["libgl1","git-lfs"]`)
	f.Set("toolchains", `["mise install go@1.23"]`)
	f.Set("env", `{"GOFLAGS":"-mod=mod"}`)
	f.Set("dockerfile_override", "FROM base\nRUN true\n")
	f.Set("tag", "orchicon-runtime-go-node:latest")
	run(t, m, submit(t, m, "tag"))

	if len(p.imgCreated) != 1 {
		t.Fatalf("CreateRuntimeImage calls = %d", len(p.imgCreated))
	}
	cr := p.imgCreated[0]
	if cr.GetName() != "Go + Node" || cr.GetAptPackages() != `["libgl1","git-lfs"]` ||
		cr.GetToolchains() != `["mise install go@1.23"]` || cr.GetEnv() != `{"GOFLAGS":"-mod=mod"}` ||
		cr.GetDockerfileOverride() != "FROM base\nRUN true\n" || cr.GetTag() != "orchicon-runtime-go-node:latest" {
		t.Fatalf("create image request = %+v", cr)
	}

	// edit the spec (prefilled from the real image, version carried)
	load(t, m, srcImages)
	run(t, m, press(t, m, "e"))
	f = m.ActiveForm()
	if f == nil {
		t.Fatal("e must open the image edit form")
	}
	if f.Values["name"] != "existing" {
		t.Fatalf("image edit form must prefill: %+v", f.Values)
	}
	f.Set("toolchains", `["npm install -g typescript"]`)
	f.Set("description", "go + node")
	run(t, m, submit(t, m, "tag"))
	up := p.imgUpdated[len(p.imgUpdated)-1]
	if up.GetId() != "img-1" || up.GetToolchains() != `["npm install -g typescript"]` || up.GetDescription() != "go + node" {
		t.Fatalf("update image request = %+v", up)
	}
	if up.GetVersion() != 3 {
		t.Fatalf("update must carry the spec version for optimistic concurrency, got %d", up.GetVersion())
	}

	// delete (Confirm-gated)
	press(t, m, kit2.DeleteChord)
	if !m.DialogOpen() {
		t.Fatal("image delete must be confirmed")
	}
	if len(p.imgDeleted) != 0 {
		t.Fatal("no delete may be sent before confirmation")
	}
	run(t, m, press(t, m, "enter"))
	if len(p.imgDeleted) != 1 || p.imgDeleted[0] != "img-1" {
		t.Fatalf("imgDeleted = %v", p.imgDeleted)
	}
	load(t, m, srcImages)
	for _, it := range itemsOf(m, srcImages) {
		if it.ID == "img-1" {
			t.Fatal("a deleted image must leave the list")
		}
	}
}

func TestRuntimeImageBuildStreamsLogsAndTransitions(t *testing.T) {
	p := newPlane()
	img := p.seedImage("img-1", "go-node", apiv1.RuntimeImageStatus_RUNTIME_IMAGE_STATUS_DRAFT)
	p.buildChunks = []*apiv1.BuildRuntimeImageResponse{
		{Log: "step 1/3: apt-get install libgl1\n"},
		{Log: "step 2/3: mise install go@1.23\n"},
		{Log: "step 3/3: commit layer\n"},
	}
	m := newModel(t, p)
	m.SelectSource(srcImages)
	load(t, m, srcImages)

	cmd := press(t, m, "b")
	if cmd == nil {
		t.Fatal("b must start the build")
	}
	if !m.Building() {
		// The open cmd has not landed yet; drive it.
		run(t, m, cmd)
		cmd = nil
	}
	if !m.Building() {
		t.Fatal("the build stream must be open after b")
	}
	if len(p.builds) != 1 {
		t.Fatalf("BuildRuntimeImage calls = %d", len(p.builds))
	}
	if p.builds[0].GetId() != "img-1" || p.builds[0].GetVersion() != img.GetVersion() {
		t.Fatalf("build request = %+v (want the spec version)", p.builds[0])
	}
	// The row reflects the in-flight transition.
	if meta := metaOf(itemsOf(m, srcImages), "img-1"); meta != "building" {
		t.Fatalf("row meta during build = %q, want building", meta)
	}

	// Drain the stream (each read is one cmd → one msg).
	drainBuild(t, m, cmd)

	if m.Building() {
		t.Fatal("the build must end on the terminal status")
	}
	log := m.BuildLog()
	for _, want := range []string{"step 1/3", "step 2/3", "step 3/3", "build done"} {
		if !strings.Contains(log, want) {
			t.Errorf("live build log missing %q:\n%s", want, log)
		}
	}
	if m.buildStatus != "ready" {
		t.Fatalf("build status = %q, want ready", m.buildStatus)
	}
	// The rendered frame carries the streamed log (the detail pane).
	if view := m.View(); !strings.Contains(view, "step 2/3") {
		t.Error("the live build log must render in the frame")
	}
	// ready transition is reflected in the row after the reconcile.
	load(t, m, srcImages)
	if meta := metaOf(itemsOf(m, srcImages), "img-1"); !strings.HasPrefix(meta, "ready") {
		t.Fatalf("row meta after build = %q, want ready", meta)
	}
	p.mu.Lock()
	if p.images["img-1"].GetStatus() != apiv1.RuntimeImageStatus_RUNTIME_IMAGE_STATUS_READY {
		t.Fatalf("the image must be ready, got %v", p.images["img-1"].GetStatus())
	}
	p.mu.Unlock()
}

// drainBuild executes build cmds until the stream terminates.
func drainBuild(t *testing.T, m *Model, first tea.Cmd) {
	t.Helper()
	cmd := first
	if cmd == nil {
		cmd = m.readBuildChunk()
	}
	for i := 0; i < 40 && m.Building(); i++ {
		if cmd == nil {
			t.Fatal("the build stalled with no read cmd armed")
		}
		msg := cmd()
		if msg == nil {
			t.Fatal("build cmd produced no message")
		}
		_, next := m.Update(msg)
		cmd = next
	}
	if m.Building() {
		t.Fatal("the build never terminated")
	}
}

// ------------- detail rendering + validation -------------

func TestDetailRendersKindBadgePillAndAcceptanceCriteria(t *testing.T) {
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	p.addItem(&apiv1.WorkItem{
		Id: "wi-1", Title: "Item", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK, ProjectId: "proj-1",
		Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_RUNNING, Description: "the description body",
		AcceptanceCriteria: "AC1: it must work", Priority: 4, Budgets: `{"usd":2}`,
		RuntimeImage: "orchicon-runtime-go:latest", AutoStartWorkflow: boolPtr(true),
	})
	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)

	msg := m.RequestDetail(srcWorkItems, "wi-1")()
	m.Update(msg)
	view := m.View()
	for _, want := range []string{"the description body", "acceptance criteria:", "AC1: it must work", "running", "[task]", `{"usd":2}`} {
		if !strings.Contains(view, want) {
			t.Errorf("work-item detail missing %q", want)
		}
	}
}

func TestCreateFormValidationBlocksSubmit(t *testing.T) {
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	run(t, m, press(t, m, "n"))
	f := m.ActiveForm()
	if f == nil {
		t.Fatal("n must open the create form")
	}
	f.Set("title", "Broken")
	f.Set("budgets", "{not json") // the JSON field type must reject this
	if cmd := submit(t, m, "auto_start"); cmd != nil {
		t.Fatal("an invalid form must not submit")
	}
	if m.ActiveForm() == nil || len(f.Errors) == 0 {
		t.Fatalf("the form must stay open and name the failure (errors=%v submitErr=%q)", f.Errors, f.SubmitErr)
	}
	if len(p.created) != 0 {
		t.Fatal("no RPC may be sent for an invalid form")
	}
	press(t, m, "esc")
	if m.ActiveForm() != nil || len(p.created) != 0 {
		t.Fatal("esc must cancel the form with no side effects")
	}
}

func TestEmptyStates(t *testing.T) {
	// Two panes render at a time (focused source + detail), so each source's
	// empty state is asserted by focusing it.
	m := newModel(t, newPlane())
	for _, src := range []string{srcProjects, srcWorkItems, srcImages} {
		load(t, m, src)
	}
	for _, tc := range []struct{ src, want string }{
		{srcProjects, "no projects yet"},
		{srcWorkItems, "no work items in this view"},
		{srcImages, "no runtime images yet"},
	} {
		if !m.SelectSource(tc.src) {
			t.Fatalf("source %q not selectable", tc.src)
		}
		if view := m.View(); !strings.Contains(view, tc.want) {
			t.Errorf("focused pane %q missing its empty state: %q", tc.src, tc.want)
		}
	}
}

func TestBuildStatusHelpers(t *testing.T) {
	if got := kindBadge(apiv1.WorkItemKind_WORK_ITEM_KIND_SUBTASK); got != "subtask" {
		t.Fatalf("kind badge = %q", got)
	}
	if got := statusPill(apiv1.WorkItemStatus_WORK_ITEM_STATUS_BLOCKED); got != "blocked" {
		t.Fatalf("state pill = %q", got)
	}
	if got := strconv.Itoa(len(descendants([]*apiv1.WorkItem{
		{Id: "a"}, {Id: "b", ParentId: "a"}, {Id: "c", ParentId: "b"},
	}, "a"))); got != "2" {
		t.Fatalf("descendants = %s", got)
	}
}

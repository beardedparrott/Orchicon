package work

// projects.go — Projects: create (CreateProject), edit the title / goals / project_dir
// (UpdateProject), and set-create the project directory.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/mutate"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/screens/mcpforms"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
)

// project form modes.
const (
	formCreateProject = "project-create"
	formEditProject   = "project-edit"
	formProjectDir    = "project-dir"
)

type projectFormMsg struct {
	mode    string
	project *apiv1.Project
	dirInfo string
	err     error
	// data is everything the form needs beyond the project row: the runtime-image options and the MCP
	// selection.
	data projectFormData
}

// projectFormData is what the project form cannot get from the project row itself.
type projectFormData struct {
	images []kit2.Option
}

// prepProjectForm fetches what the form needs and opens it.
func (m *Model) prepProjectForm(mode, id string) tea.Cmd {
	m.formLoading = true
	cl := m.cl
	cached := m.images
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		msg := projectFormMsg{mode: mode}
		if id != "" {
			resp, err := cl.Projects.GetProject(ctx, connect.NewRequest(&apiv1.GetProjectRequest{Id: id}))
			if err != nil {
				return projectFormMsg{mode: mode, err: err}
			}
			msg.project = resp.Msg.GetProject()
		}
		msg.data.images = cached
		if msg.data.images == nil && cl.Images != nil {
			if lr, err := cl.Images.ListRuntimeImages(ctx, connect.NewRequest(&apiv1.ListRuntimeImagesRequest{PageSize: 100})); err == nil {
				for _, img := range lr.Msg.GetRuntimeImages() {
					msg.data.images = append(msg.data.images, kit2.Option{Value: img.GetTag(), Label: img.GetName() + " (" + img.GetTag() + ")"})
				}
			}
		}
		// NO MCP LIST READ ANY MORE.
		return msg
	}
}

// prepEditProject fetches the selected project so the edit form is
// prefilled from real values.
func (m *Model) prepEditProject(mode string) tea.Cmd {
	it, ok := m.ActiveItem()
	if !ok {
		return nil
	}
	return m.prepProjectForm(mode, it.ID)
}

// ProjectFormFields is the project form's field list — ONE definition, shared by create, edit and
// (with a directory appended) orch's launch prompt.
func ProjectFormFields(p *apiv1.Project, images []kit2.Option) []kit2.FieldSpec {
	initial := func(fn func() string) string {
		if p == nil {
			return ""
		}
		return fn()
	}
	current := initial(p.GetDefaultRuntimeImage)
	// MODE-AWARE, matching the GUI and the work-item picker: a LOCAL project's runs execute on the
	// host, so a default image cannot apply.
	localOnly := func(values map[string]string) bool {
		return strings.EqualFold(strings.TrimSpace(values["execution_mode"]), "runtime")
	}
	_ = localOnly

	return []kit2.FieldSpec{
		{Name: "name", Label: "Name", Kind: kit2.KText, Required: true, Placeholder: "Orchicon", Initial: initial(p.GetName)},
		{Name: "slug", Label: "Slug", Kind: kit2.KText, Placeholder: "orchicon", Initial: initial(p.GetSlug)},
		{Name: "goals", Label: "Goals", Kind: kit2.KText, Placeholder: "key=value, key2=value2", Initial: initial(func() string { return goalsText(p.GetGoals()) })},
		func() kit2.FieldSpec {
			f := runtimeImageField(current, images)
			// ONLY WHEN THE MODE IS RUNTIME.
			f.Visible = func(values map[string]string) bool {
				return strings.EqualFold(strings.TrimSpace(values["execution_mode"]), "runtime")
			}
			return f
		}(),
		{
			Name: "git_strategy", Label: "Git strategy", Kind: kit2.KSelect,
			Options: gitStrategyOptions(), Initial: gitStrategyField(p),
		},
		{
			Name: "execution_mode", Label: "Execution mode", Kind: kit2.KSelect,
			Options: executionModeOptions(), Initial: executionModeField(p),
		},
		{
			Name: "max_concurrent_runs", Label: "Max concurrent runs", Kind: kit2.KNumber,
			Placeholder: "0 = no additional restriction", Initial: maxConcurrentField(p),
		},
		{
			Name: "context_files", Label: contextFilesLabel, Kind: kit2.KTextArea,
			Placeholder: contextFilesPlaceholder, Initial: initial(func() string { return contextFilesText(p.GetContextFiles()) }),
		},
		// SKILL FILES: the SAME path-list idiom as context_files, beside it.
		{
			Name: "skill_files", Label: skillFilesLabel, Kind: kit2.KTextArea,
			Placeholder: skillFilesPlaceholder, Initial: initial(func() string { return contextFilesText(p.GetSkillFiles()) }),
		},
	}
}

// contextFilesLabel and contextFilesPlaceholder carry the guidance for the one field whose meaning
// is not obvious from its name.
const (
	contextFilesLabel = "Context files (in project dir, one per line)"
	// A DIRECTORY IS ALLOWED and is the more useful choice for most projects, which is worth saying —
	// the field's name reads like it wants files only.
	contextFilesPlaceholder = "(dirs read in full)"
)

// WRITTEN DOWN.
const (
	skillFilesLabel = "Skill files (in project dir, one per line)"
	// Same directory note as context_files: a skill artifact may be a directory.
	skillFilesPlaceholder = "(dirs read in full)"
)

// ParseSkillFiles reads the skill_files field with the SAME rule as context_files: one path per
// line, commas also accepted, blanks dropped.
func ParseSkillFiles(s string) []string { return ParseContextFiles(s) }

// runtimeImageField is the project's default runtime image.
func runtimeImageField(current string, images []kit2.Option) kit2.FieldSpec {
	if len(images) == 0 {
		return kit2.FieldSpec{
			Name: "default_runtime_image", Label: "Default runtime image", Kind: kit2.KText,
			Placeholder: "empty = inherit tenant/base", Initial: current,
		}
	}
	opts := append([]kit2.Option{{Value: "", Label: "— inherit tenant/base —"}}, images...)
	return kit2.FieldSpec{
		Name: "default_runtime_image", Label: "Default runtime image", Kind: kit2.KPicker,
		Options: pickerOptsWithCurrent(opts, current, "image "),
		Initial: current,
	}
}

// ProjectMCPDefinitionsField is the CREATE form's MCP seed: a JSON array of inline specs, applied
// as OWNED ROWS for the new project after CreateProject returns.
func ProjectMCPDefinitionsField() kit2.FieldSpec {
	return kit2.FieldSpec{
		Name: "mcp_servers", Label: "MCP definitions (JSON)", Kind: kit2.KJSON,
		Placeholder: `[{"id":"github","type":"stdio","command":["npx","-y","@modelcontextprotocol/server-github"],"env":{"GITHUB_TOKEN":"${GITHUB_TOKEN}"}}]`,
	}
}

// NOTE: setProjectMCPServers was REMOVED.

// newProjectCreateForm builds the create form from the shared field list.
func (m *Model) newProjectCreateForm() *kit2.Form {
	return m.newProjectCreateFormWith(projectFormData{})
}

// newProjectCreateFormWith adds the option lists that need a round trip (the runtime-image
// picker) and the owned-MCP-definition seed.
func (m *Model) newProjectCreateFormWith(d projectFormData) *kit2.Form {
	specs := append(ProjectFormFields(nil, d.images), ProjectMCPDefinitionsField())
	f := kit2.NewForm("New project", specs...)
	m.wireProjectForm(f, formCreateProject, "")
	return f
}

// newProjectEditForm builds the edit form: the shared fields PREFILLED, plus the
// directory (which CreateProject cannot carry and this path therefore owns).
func (m *Model) newProjectEditForm(p *apiv1.Project) *kit2.Form {
	return m.newProjectEditFormWith(p, projectFormData{})
}

// newProjectEditFormWith adds the option lists that need a round trip.
func (m *Model) newProjectEditFormWith(p *apiv1.Project, d projectFormData) *kit2.Form {
	specs := append(ProjectFormFields(p, d.images), kit2.FieldSpec{
		Name: "project_dir", Label: "Project dir", Kind: kit2.KText,
		Initial: p.GetProjectDir(), Placeholder: "/home/me/projects/orchicon",
	})

	f := kit2.NewForm("Edit project", specs...)
	m.wireProjectForm(f, formEditProject, p.GetId())
	return f
}

// gitStrategyOptions are the selectable values, in the order the GUI offers them.
func gitStrategyOptions() []kit2.Option {
	return []kit2.Option{
		{Value: "local", Label: "local — commit on a local branch"},
		{Value: "pr", Label: "pr — open a pull request"},
		{Value: "none", Label: "none — do not commit"},
	}
}

// executionModeOptions mirror the GUI's select: local is the default, runtime
// the opt-in per-run container.
func executionModeOptions() []kit2.Option {
	return []kit2.Option{
		{Value: "local", Label: "local — in-process (default)"},
		{Value: "runtime", Label: "runtime — per-run container (opt-in)"},
	}
}

// gitStrategyField reads a project's git strategy as the field's string value.
func gitStrategyField(p *apiv1.Project) string {
	if p == nil {
		return "local"
	}
	if s := gitStrategyString(p.GetGitStrategy()); s != "" {
		return s
	}
	if s := goalValue(p.GetGoals(), "__git_strategy"); s != "" {
		return s
	}
	return "local"
}

// executionModeField reads a project's execution mode, defaulting to local (the
// proto's and the server's default): containers are opt-in.
func executionModeField(p *apiv1.Project) string {
	if p == nil {
		return "local"
	}
	if s := executionModeString(p.GetExecutionMode()); s != "" {
		return s
	}
	return "local"
}

// maxConcurrentField renders the override as the field's text. 0 is the proto's
// "no additional restriction", so it is what an unset override means.
func maxConcurrentField(p *apiv1.Project) string {
	if p == nil {
		return "0"
	}
	return strconv.Itoa(int(p.GetMaxConcurrentRuns()))
}

func gitStrategyString(e apiv1.GitStrategy) string {
	switch e {
	case apiv1.GitStrategy_GIT_STRATEGY_LOCAL:
		return "local"
	case apiv1.GitStrategy_GIT_STRATEGY_PR:
		return "pr"
	case apiv1.GitStrategy_GIT_STRATEGY_NONE:
		return "none"
	}
	return "" // UNSPECIFIED — let the caller decide the default
}

func gitStrategyEnum(s string) apiv1.GitStrategy {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "local":
		return apiv1.GitStrategy_GIT_STRATEGY_LOCAL
	case "pr":
		return apiv1.GitStrategy_GIT_STRATEGY_PR
	case "none":
		return apiv1.GitStrategy_GIT_STRATEGY_NONE
	}
	return apiv1.GitStrategy_GIT_STRATEGY_UNSPECIFIED
}

func executionModeString(e apiv1.ExecutionMode) string {
	switch e {
	case apiv1.ExecutionMode_EXECUTION_MODE_RUNTIME:
		return "runtime"
	case apiv1.ExecutionMode_EXECUTION_MODE_LOCAL:
		return "local"
	}
	return ""
}

func executionModeEnum(s string) apiv1.ExecutionMode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "runtime":
		return apiv1.ExecutionMode_EXECUTION_MODE_RUNTIME
	case "local":
		return apiv1.ExecutionMode_EXECUTION_MODE_LOCAL
	}
	return apiv1.ExecutionMode_EXECUTION_MODE_UNSPECIFIED
}

// contextFilesText renders a project's context files as the field's text — one path
// per line, which is also the shape the server stores (a list of paths).
func contextFilesText(files []string) string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return strings.Join(out, "\n")
}

// ParseContextFiles reads the field's text as a list of paths: one per line, with commas also
// accepted because a single-line paste of "a.
func ParseContextFiles(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		for _, part := range strings.Split(line, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// goalValue reads one key out of a project's goals JSON document. The document is a
// flat object (project.convertGoalsToJSON), which is also how the GUI reads it.
func goalValue(raw, key string) string {
	m := goalsMap(raw)
	return m[key]
}

// goalsMap decodes a goals document, tolerating anything unexpected by returning an
// empty map: a malformed document must not block the form from opening.
func goalsMap(raw string) map[string]string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var any map[string]any
	if err := json.Unmarshal([]byte(raw), &any); err != nil {
		return nil
	}
	out := make(map[string]string, len(any))
	for k, v := range any {
		out[k] = fmt.Sprint(v)
	}
	return out
}

// newProjectDirForm sets/creates the project directory.
func (m *Model) newProjectDirForm(p *apiv1.Project) *kit2.Form {
	f := kit2.NewForm("Set project directory",
		kit2.FieldSpec{Name: "project_dir", Label: "Directory", Kind: kit2.KText, Required: true, Initial: p.GetProjectDir(), Placeholder: "/home/me/projects/orchicon"},
	)
	m.wireProjectForm(f, formProjectDir, p.GetId())
	return f
}

// parseMaxConcurrentRuns reads the override field.
func parseMaxConcurrentRuns(s string) int32 {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0
	}
	return int32(n)
}

// projectUpdater is the slice of the project client applyProjectPostCreate needs, so
// the two-call create is testable without a full client.
type projectUpdater interface {
	UpdateProject(context.Context, *connect.Request[apiv1.UpdateProjectRequest]) (*connect.Response[apiv1.UpdateProjectResponse], error)
}

// projectMCPCreator is the slice of the MCP client applyProjectPostCreate needs to materialize the
// create form's owned definitions.
type projectMCPCreator interface {
	CreateMCPServer(context.Context, *connect.Request[apiv1.MCPServerCreateRequest]) (*connect.Response[apiv1.MCPServerCreateResponse], error)
}

// applyProjectPostCreate attaches what CreateProject cannot carry: the concurrency override, the
// context files, the skill files, and the owned MCP definitions the create form seeded.
func applyProjectPostCreate(ctx context.Context, cl projectUpdater, mcp projectMCPCreator, id string, maxRuns int32, contextFiles, skillFiles []string, mcpDefs []mcpforms.InlineSpec) error {
	if id == "" {
		return nil
	}
	if maxRuns == 0 && len(contextFiles) == 0 && len(skillFiles) == 0 {
		// Nothing for the follow-up UPDATE. The MCP definitions still have to be created.
		return createOwnedMCPServers(ctx, mcp, id, mcpDefs)
	}
	req := &apiv1.UpdateProjectRequest{Id: id}
	if maxRuns > 0 {
		req.MaxConcurrentRuns = &maxRuns
	}
	if len(contextFiles) > 0 {
		req.ContextFiles = &apiv1.ContextFiles{Files: contextFiles}
	}
	if len(skillFiles) > 0 {
		req.SkillFiles = &apiv1.ContextFiles{Files: skillFiles}
	}
	if _, err := cl.UpdateProject(ctx, connect.NewRequest(req)); err != nil {
		return err
	}
	return createOwnedMCPServers(ctx, mcp, id, mcpDefs)
}

// createOwnedMCPServers materializes the create form's definitions as project-owned rows.
// An empty array creates nothing (the same rule the update follows).
func createOwnedMCPServers(ctx context.Context, mcp projectMCPCreator, projectID string, specs []mcpforms.InlineSpec) error {
	if len(specs) == 0 {
		return nil
	}
	if mcp == nil {
		return fmt.Errorf("no MCP client — the project's MCP definitions were not saved")
	}
	for _, spec := range specs {
		req := InlineSpecToCreateRequest(spec, projectID)
		if _, err := mcp.CreateMCPServer(ctx, connect.NewRequest(req)); err != nil {
			return fmt.Errorf("MCP definition %q could not be saved: %w", spec.ID, err)
		}
	}
	return nil
}

// InlineSpecToCreateRequest turns an inline spec (the shape db.MCPServerFromPermissions parses)
// into an OWNED create request.
func InlineSpecToCreateRequest(spec mcpforms.InlineSpec, projectID string) *apiv1.MCPServerCreateRequest {
	enabled := true
	if spec.Enabled != nil {
		enabled = *spec.Enabled
	}
	req := &apiv1.MCPServerCreateRequest{
		Name:        spec.ID,
		ProjectId:   projectID,
		Transport:   apiv1.MCPServerTransport_MCP_SERVER_TRANSPORT_STDIO,
		Enabled:     enabled,
		CatalogSlug: "",
	}
	if spec.Type == "http" || spec.URL != "" {
		req.Transport = apiv1.MCPServerTransport_MCP_SERVER_TRANSPORT_STREAMABLE_HTTP
		req.Url = spec.URL
		req.Headers = spec.Headers
		return req
	}
	if len(spec.Command) > 0 {
		req.Command = spec.Command[0]
		req.Args = spec.Command[1:]
	}
	req.Env = spec.Env
	return req
}

// THE PROJECT'S MCP FORMS ARE THE MODAL'S NOW.

// mcpRowSummary renders one owned MCP row for the project detail pane: transport
// plus command-or-url, the same facts the GUI card shows.
func mcpRowSummary(s *apiv1.MCPServer) string {
	transport := "stdio"
	if s.GetTransport() == apiv1.MCPServerTransport_MCP_SERVER_TRANSPORT_STREAMABLE_HTTP {
		transport = "http"
	}
	where := s.GetCommand()
	if transport == "http" {
		where = s.GetUrl()
	}
	state := "enabled"
	if !s.GetEnabled() {
		state = "disabled"
	}
	return transport + " · " + where + " · " + state
}

// wireProjectForm installs the submit handler.
func (m *Model) wireProjectForm(f *kit2.Form, mode, id string) {
	f.Focused = true
	f.Width = 70
	f.OnSubmit = func(v map[string]string, multi map[string][]string) (tea.Cmd, error) {
		name := strings.TrimSpace(v["name"])
		goals := ParseGoals(v["goals"])
		contextFiles := ParseContextFiles(v["context_files"])
		skillFiles := ParseSkillFiles(v["skill_files"])
		maxRuns := parseMaxConcurrentRuns(v["max_concurrent_runs"])
		// The MCP field is an OWNED-DEFINITION seed (create only) — no selection is
		// written, because selection IS ownership (mcp_servers.project_id).
		mcpDefs := mcpforms.ParseInline(v["mcp_servers"])
		_ = multi
		switch mode {
		case formCreateProject:
			req := &apiv1.CreateProjectRequest{
				Name:                name,
				Slug:                strings.TrimSpace(v["slug"]),
				Goals:               goals,
				DefaultRuntimeImage: strings.TrimSpace(v["default_runtime_image"]),
				GitStrategy:         gitStrategyEnum(v["git_strategy"]),
				ExecutionMode:       executionModeEnum(v["execution_mode"]),
			}
			cl := m.cl
			return m.Mutate(mutate.Request{
				Name: "create project " + strings.TrimSpace(name), Source: srcProjects,
				Rollback: func() { m.Refresh(srcProjects) },
				Do: func(ctx context.Context) error {
					created, err := cl.Projects.CreateProject(ctx, connect.NewRequest(req))
					if err != nil {
						return err
					}
					// THE SECOND CALL IS FORCED BY THE PROTO, not chosen: CreateProject accepts no max_concurrent_runs,
					// no context_files and no skill_files.
					if err := applyProjectPostCreate(ctx, cl.Projects, cl.MCP, created.Msg.GetProject().GetId(), maxRuns, contextFiles, skillFiles, mcpDefs); err != nil {
						return err
					}
					return nil
				},
			}), nil

		case formEditProject:
			req := &apiv1.UpdateProjectRequest{
				Id:                  id,
				Name:                strPtr(name),
				Slug:                strPtr(strings.TrimSpace(v["slug"])),
				Goals:               &apiv1.GoalFields{Fields: goals},
				ProjectDir:          strPtr(strings.TrimSpace(v["project_dir"])),
				DefaultRuntimeImage: strPtr(strings.TrimSpace(v["default_runtime_image"])),
				GitStrategy:         gitStrategyEnum(v["git_strategy"]).Enum(),
				ExecutionMode:       executionModeEnum(v["execution_mode"]).Enum(),
				MaxConcurrentRuns:   &maxRuns,
				// ALWAYS SENT, so an emptied field CLEARS the selection: the proto reads an empty list as "clear"
				// ("empty files list clears the selection").
				ContextFiles: &apiv1.ContextFiles{Files: contextFiles},
				// skill_files is an `optional ContextFiles` (field-mask semantics), so it is ALWAYS sent from the
				// edit form — an emptied field CLEARS, the same rule context_files follows.
				SkillFiles: &apiv1.ContextFiles{Files: skillFiles},
			}
			return m.Mutate(mutate.Request{
				Name: "save project " + name, Source: srcProjects,
				Rollback: func() { m.Refresh(srcProjects) },
				Do: func(ctx context.Context) error {
					if _, err := m.cl.Projects.UpdateProject(ctx, connect.NewRequest(req)); err != nil {
						return err
					}
					return nil
				},
			}), nil

		case formProjectDir:
			dir := strings.TrimSpace(v["project_dir"])
			if dir == "" {
				return nil, fmt.Errorf("a directory path is required")
			}
			cl := m.cl
			return m.Mutate(mutate.Request{
				Name: "set project directory", Source: srcProjects,
				Rollback: func() { m.Refresh(srcProjects) },
				Do: func(ctx context.Context) error {
					if _, err := cl.Projects.UpdateProject(ctx, connect.NewRequest(&apiv1.UpdateProjectRequest{
						Id:         id,
						ProjectDir: &dir,
					})); err != nil {
						return err
					}
					// Probe the directory: ListProjectFiles resolves
					// project_dir server-side, so a bad path fails here.
					_, err := cl.Projects.ListProjectFiles(ctx, connect.NewRequest(&apiv1.ListProjectFilesRequest{Id: id}))
					return err
				},
			}), nil
		}
		return nil, nil
	}
}

// projectActions is the Projects pane's entity-bound action set. This function MUST NOT set
// m.formLoading.
func (m *Model) projectActions() []kit2.Action {
	it, ok := m.ActiveItem()
	if !ok {
		return nil
	}
	id := it.ID
	cl := m.cl
	acts := []kit2.Action{{
		Label: "create project dir", Key: "d", Source: srcProjects,
		Do: func(ctx context.Context) error {
			// Resolve the project's configured directory and list it, which
			// is what materializes + validates it server-side.
			p, err := cl.Projects.GetProject(ctx, connect.NewRequest(&apiv1.GetProjectRequest{Id: id}))
			if err != nil {
				return err
			}
			if p.Msg.GetProject().GetProjectDir() == "" {
				return fmt.Errorf("set the project directory first (e)")
			}
			_, err = cl.Projects.ListProjectFiles(ctx, connect.NewRequest(&apiv1.ListProjectFilesRequest{Id: id}))
			return err
		},
	}}
	// OFFERED ONLY WHEN IT CAN SUCCEED: ActivateProject's UPDATE carries `AND status = 'drafting'`, so
	// offering it on an active project would be an action that reports a failure for doing the right.
	if projectNeedsActivation(it.Meta) {
		acts = append(acts, kit2.Action{
			Label: "activate", Key: "a", Source: srcProjects,
			Do: func(ctx context.Context) error {
				_, err := cl.Projects.ActivateProject(ctx, connect.NewRequest(&apiv1.ActivateProjectRequest{Id: id}))
				return err
			},
		})
	}
	// DELETE — the operator's report: "There is no ctrl+x delete for single or bulk on projects." The
	// RPC existed and the TUI never offered it.
	acts = append(acts, kit2.Action{
		Label: "delete", Key: kit2.DeleteChord, Danger: true, Source: srcProjects,
		Confirm: "Delete " + it.Title + "?\n\nThis permanently removes the project. It cannot be undone.",
		Do: func(ctx context.Context) error {
			_, err := cl.Projects.DeleteProject(ctx, connect.NewRequest(&apiv1.DeleteProjectRequest{Id: id}))
			return err
		},
	})
	return acts
}

// bulkProjectActions are the operations that make sense on a whole projects selection.
func (m *Model) bulkProjectActions(ids []string) []kit2.Action {
	n := len(ids)
	count := fmt.Sprintf("%d", n)
	label := func(verb string) string { return verb + " " + count + " selected" }
	if m.cl == nil || m.cl.Projects == nil {
		return []kit2.Action{{
			Label: "no project client", Source: srcProjects,
			Do: func(context.Context) error { return fmt.Errorf("no project client") },
		}}
	}
	cl := m.cl.Projects
	return []kit2.Action{
		{
			Label: label("delete"), Key: kit2.DeleteChord, Danger: true, Source: srcProjects,
			Confirm: "Delete " + count + " projects?\n\nThis permanently removes each one. It cannot be undone.",
			Do: func(ctx context.Context) error {
				failed := 0
				for _, id := range ids {
					if _, err := cl.DeleteProject(ctx, connect.NewRequest(&apiv1.DeleteProjectRequest{Id: id})); err != nil {
						failed++
					}
				}
				if failed > 0 {
					return fmt.Errorf("deleted %d of %d — %d failed", n-failed, n, failed)
				}
				return nil
			},
		},
		{
			Label: "clear selection", Key: "esc", Source: srcProjects,
			Do: func(context.Context) error { return nil },
			Apply: func() {
				m.Base.ClearMarks()
				m.notice = "selection cleared"
			},
		},
	}
}

// projectNeedsActivation reports whether a projects row is in `drafting` — i.e. whether
// ActivateProject can succeed on it.
func projectNeedsActivation(meta string) bool {
	return meta == "drafting" || strings.HasPrefix(meta, "drafting · ")
}

// goalsText renders a project's goals JSON document as the form's editable "key=value, key2=value2"
// text.
func goalsText(raw string) string {
	m := goalsMap(raw)
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		if k == "__git_strategy" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ", ")
}

// ParseGoals parses the goals field's "key=value, key2=value2" text into the wire shape.
func ParseGoals(v string) []*apiv1.GoalField {
	var out []*apiv1.GoalField
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, val, found := strings.Cut(part, "=")
		if !found {
			out = append(out, &apiv1.GoalField{Key: strings.TrimSpace(part)})
			continue
		}
		out = append(out, &apiv1.GoalField{Key: strings.TrimSpace(k), Value: strings.TrimSpace(val)})
	}
	return out
}

var _ = screenkit.FmtTime

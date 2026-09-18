package work

// projects.go — Projects: create (CreateProject), edit the title / goals /
// project_dir (UpdateProject), and set-create the project directory.
//
// Note on the directory: ProjectService exposes no CreateProjectDirectory
// RPC (its 10 RPCs are Create/Get/List/Update/Archive/Delete/Pause/
// Activate/StreamProjectEvents/ListProjectFiles). The TUI therefore sets
// project_dir through UpdateProject and then PROBES the directory with
// ListProjectFiles — a bad path surfaces immediately as an error instead of
// failing later at worker dispatch.
//
// CORRECTION (this comment used to claim the probe MATERIALIZED the directory —
// "the server materializes the directory when the files endpoint resolves it" —
// and it does not): the read path is os.Stat + os.ReadDir only
// (internal/project/listDirectory), so the probe is a genuine existence check and
// creates nothing. That matters beyond pedantry, because the same call is what
// launch.go uses to ask whether the PLANE can see a directory: a probe that
// created what it was probing for would answer its own question with a yes.

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
}

// prepEditProject fetches the selected project so the edit form is
// prefilled from real values.
func (m *Model) prepEditProject(mode string) tea.Cmd {
	it, ok := m.ActiveItem()
	if !ok {
		return nil
	}
	m.formLoading = true
	id := it.ID
	cl := m.cl
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		resp, err := cl.Projects.GetProject(ctx, connect.NewRequest(&apiv1.GetProjectRequest{Id: id}))
		if err != nil {
			return projectFormMsg{mode: mode, err: err}
		}
		return projectFormMsg{mode: mode, project: resp.Msg.GetProject()}
	}
}

// ProjectFormFields is the project form's field list — ONE definition, shared by
// create, edit and (with a directory appended) orch's launch prompt, so the three
// cannot drift apart in what they offer.
//
// p is nil for a CREATE (defaults apply) and the project being edited otherwise.
// CreateProject accepts only a subset of these (name/slug/goals/git_strategy/
// default_runtime_image/execution_mode), so the create path applies the rest —
// max_concurrent_runs and context_files — with a FOLLOWING UpdateProject. That is
// the same two-call shape the GUI uses, forced by the proto rather than chosen.
//
// THERE IS NO project_dir FIELD HERE. CreateProject has no directory, so the hosts
// that need one (edit, and the launch prompt) append it themselves; see
// newProjectEditForm.
func ProjectFormFields(p *apiv1.Project) []kit2.FieldSpec {
	initial := func(fn func() string) string {
		if p == nil {
			return ""
		}
		return fn()
	}
	return []kit2.FieldSpec{
		{Name: "name", Label: "Name", Kind: kit2.KText, Required: true, Placeholder: "Orchicon", Initial: initial(p.GetName)},
		{Name: "slug", Label: "Slug", Kind: kit2.KText, Placeholder: "orchicon", Initial: initial(p.GetSlug)},
		{Name: "goals", Label: "Goals", Kind: kit2.KText, Placeholder: "key=value, key2=value2", Initial: initial(func() string { return goalsText(p.GetGoals()) })},
		{Name: "default_runtime_image", Label: "Default runtime image", Kind: kit2.KText, Placeholder: "empty = inherit tenant/base", Initial: initial(p.GetDefaultRuntimeImage)},
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
			Name: "context_files", Label: "Context files", Kind: kit2.KTextArea,
			Placeholder: "one absolute path per line", Initial: initial(func() string { return contextFilesText(p.GetContextFiles()) }),
		},
	}
}

// newProjectCreateForm builds the create form from the shared field list.
func (m *Model) newProjectCreateForm() *kit2.Form {
	f := kit2.NewForm("New project", ProjectFormFields(nil)...)
	m.wireProjectForm(f, formCreateProject, "")
	return f
}

// newProjectEditForm builds the edit form: the shared fields PREFILLED, plus the
// directory (which CreateProject cannot carry and this path therefore owns).
func (m *Model) newProjectEditForm(p *apiv1.Project) *kit2.Form {
	specs := append(ProjectFormFields(p), kit2.FieldSpec{
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

// executionModeOptions mirror the GUI's select.
func executionModeOptions() []kit2.Option {
	return []kit2.Option{
		{Value: "runtime", Label: "runtime — per-run container (isolated)"},
		{Value: "local", Label: "local — in the control plane"},
	}
}

// gitStrategyField reads a project's git strategy as the field's string value.
//
// IT FALLS BACK TO THE HIDDEN GOAL, which is not belt-and-braces: projects created
// before the typed field existed carry their strategy in a `__git_strategy` goal,
// and the GUI reads it exactly this way (it reaches into the goals JSON when the
// typed field is unset). Without the fallback, editing an older project would show
// "local" — the default — and SAVING WOULD SILENTLY CHANGE ITS STRATEGY.
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

// executionModeField reads a project's execution mode, defaulting to runtime (the
// proto's and the server's default).
func executionModeField(p *apiv1.Project) string {
	if p == nil {
		return "runtime"
	}
	if s := executionModeString(p.GetExecutionMode()); s != "" {
		return s
	}
	return "runtime"
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

// ParseContextFiles reads the field's text as a list of paths: one per line, with
// commas also accepted because a single-line paste of "a, b" is the natural mistake
// and silently treating it as one absurd path would fail validation for a reason the
// operator cannot see. Blanks are dropped so a trailing newline is harmless.
//
// Validation (absolute, no traversal, inside the project dir) is the SERVER'S —
// contextfiles.Validate / ValidateWithin. Re-implementing it here would give the TUI a
// second, subtly different rule to disagree with.
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

// parseMaxConcurrentRuns reads the override field. Anything unparseable or negative
// becomes 0 — the proto's "no additional restriction" — rather than failing the save
// over a typo the operator can simply correct, and never a negative limit the server
// would have to reject.
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

// applyProjectPostCreate attaches what CreateProject cannot carry: the concurrency
// override and the context files.
//
// BOTH ARE CONDITIONALLY SENT, which is right for a CREATE and the opposite of the edit
// path: there is nothing to clear on a project that was just made, so an empty list
// would be a no-op write and max_concurrent_runs=0 would write the value the server
// already defaulted to. An update with nothing to say is SKIPPED ENTIRELY, so a plain
// create does not depend on a second call succeeding — otherwise a failed follow-up
// would report failure for a project that exists and is perfectly usable.
func applyProjectPostCreate(ctx context.Context, cl projectUpdater, id string, maxRuns int32, contextFiles []string) error {
	if id == "" || (maxRuns == 0 && len(contextFiles) == 0) {
		return nil
	}
	req := &apiv1.UpdateProjectRequest{Id: id}
	if maxRuns > 0 {
		req.MaxConcurrentRuns = &maxRuns
	}
	if len(contextFiles) > 0 {
		req.ContextFiles = &apiv1.ContextFiles{Files: contextFiles}
	}
	_, err := cl.UpdateProject(ctx, connect.NewRequest(req))
	return err
}

// wireProjectForm installs the submit handler.
func (m *Model) wireProjectForm(f *kit2.Form, mode, id string) {
	f.Focused = true
	f.Width = 70
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		name := strings.TrimSpace(v["name"])
		goals := ParseGoals(v["goals"])
		contextFiles := ParseContextFiles(v["context_files"])
		maxRuns := parseMaxConcurrentRuns(v["max_concurrent_runs"])
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
					// THE SECOND CALL IS FORCED BY THE PROTO, not chosen: CreateProject
					// accepts no max_concurrent_runs and no context_files, so a create form
					// offering them has to apply them afterwards. Same two-call shape the GUI
					// uses for maxConcurrentRuns.
					return applyProjectPostCreate(ctx, cl.Projects, created.Msg.GetProject().GetId(), maxRuns, contextFiles)
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
				// ALWAYS SENT, so an emptied field CLEARS the selection: the proto reads an
				// empty list as "clear" ("empty files list clears the selection"), and an
				// operator emptying a prefilled list means exactly that. Sending it only when
				// non-empty would make a selection impossible to remove from the TUI.
				ContextFiles: &apiv1.ContextFiles{Files: contextFiles},
			}
			return m.Mutate(mutate.Request{
				Name: "save project " + name, Source: srcProjects,
				Rollback: func() { m.Refresh(srcProjects) },
				Do: func(ctx context.Context) error {
					_, err := m.cl.Projects.UpdateProject(ctx, connect.NewRequest(req))
					return err
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

// projectActions is the Projects pane's entity-bound action set.
//
// This function MUST NOT set m.formLoading. It used to, and that single line broke
// the whole tab bar on this pane: actionsForSelection() calls it to build the footer
// hints and the key bindings, so formLoading latched TRUE the moment a project was
// selected and NOTHING ever cleared it (the three legitimate setters are the prep*
// helpers, whose returned cmd delivers the *_formMsg that clears it).
//
// ClaimsKeys() includes formLoading, so the shell then handed EVERY key to this screen
// before any of its own routes ran:
//
//   - the Work submenu's up/down never reached menuHandleKey (the menu looked dead);
//   - Enter selected nothing, because it fell to the pane's own "load detail";
//   - shift+tab was swallowed outright — it is a global route, which sits even later
//     in the chain.
//
// That is the operator's "can't go into reverse tab once I hit the Work/Projects
// screen ... can't hit enter on any submenu under Work nor up/down on the Work menu" —
// and it is why the oddity was specific to Projects: itemActions() and imageActions()
// never set the flag. The two locals below are captured by the closures and have
// nothing to do with form preparation.
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
	// ACTIVATE — the only way out of `drafting`, and its absence made a newly created
	// project a DEAD END in the TUI.
	//
	// CreateProject always lands a project in `drafting` (deliberately: it is the gate
	// that lets a project be configured before it accepts work), and db.RequireProjectActive
	// refuses work items for anything not `active`. The GUI answers this with an Activate
	// button on the project page; the TUI had no such action at all, so a project created
	// here — including by the launch prompt — could never host a single work item and
	// nothing said why. The operator hit exactly that: "it created the project in draft
	// mode".
	//
	// OFFERED ONLY WHEN IT CAN SUCCEED: ActivateProject's UPDATE carries
	// `AND status = 'drafting'`, so offering it on an active project would be an action
	// that reports a failure for doing the right thing twice. The status is read from the
	// row's Meta — see projectNeedsActivation for that coupling.
	if projectNeedsActivation(it.Meta) {
		acts = append(acts, kit2.Action{
			Label: "activate", Key: "a", Source: srcProjects,
			Do: func(ctx context.Context) error {
				_, err := cl.Projects.ActivateProject(ctx, connect.NewRequest(&apiv1.ActivateProjectRequest{Id: id}))
				return err
			},
		})
	}
	return acts
}

// projectNeedsActivation reports whether a projects row is in `drafting` — i.e.
// whether ActivateProject can succeed on it.
//
// IT READS THE ROW'S Meta, and the coupling is stated rather than hidden:
// fetchProjects (screen.go) builds a project's Meta as "<status> · <project_dir>", so
// the status is the first token BY CONSTRUCTION. Reading a presentation string for
// state is not ideal, and the honest alternative is carrying the status as its own
// field on kit2.Item — not done here because that touches the shared item type for one
// action. IF A SECOND CALLER EVER NEEDS PROJECT STATE, PROMOTE IT TO A FIELD rather
// than re-parsing this.
//
// THE STATUS IS MATCHED AS A WHOLE TOKEN, not as a prefix. A bare HasPrefix("drafting")
// also accepts "drafting-notes", and the second half of Meta is an OPERATOR-SUPPLIED
// DIRECTORY PATH — arbitrary text that must never be read as state. That misreading was
// in an earlier version of this function, whose comment confidently claimed it "cannot
// false-positive on a directory"; the test beside it disproved the claim. The status is
// therefore either the entire string or followed by the separator fetchProjects writes
// (" · ").
func projectNeedsActivation(meta string) bool {
	return meta == "drafting" || strings.HasPrefix(meta, "drafting · ")
}

// goalsText renders a project's goals JSON document as the form's editable
// "key=value, key2=value2" text. The document is a flat object
// (project.convertGoalsToJSON), which is how the GUI reads it back too
// (Object.entries(JSON.parse(...))).
//
// THIS USED TO RETURN THE RAW JSON, and that was a silent data-corruption bug: the
// edit form prefilled `{"key":"value"}`, ParseGoals then split it on "," and "=",
// found no "=", and turned the whole document into goals whose KEY was the JSON — so
// saving an untouched edit destroyed the project's goals. Values are rendered with
// fmt.Sprint so a non-string value round-trips as its text rather than being dropped.
//
// __git_strategy IS DELIBERATELY OMITTED: it is a legacy hiding place for the git
// strategy from before that became a typed field, and it has a field of its own now
// (git_strategy, which reads this same key as a fallback). Showing it as a goal would
// put an internal marker in front of the operator and offer it for editing twice.
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

// ParseGoals parses the goals field's "key=value, key2=value2" text into the
// wire shape. An empty input clears the goals (the proto's empty-fields
// semantics).
//
// EXPORTED so a second host runs the SAME parsing — a goal list is part of the
// create request's meaning, and two parsers would eventually disagree about an
// edge (a bare key, a value containing '=').
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

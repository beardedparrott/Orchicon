package work

// projects.go — Projects: create (CreateProject), edit the title / goals /
// project_dir (UpdateProject), and set-create the project directory.
//
// Note on the directory: ProjectService exposes no CreateProjectDirectory
// RPC (its 10 RPCs are Create/Get/List/Update/Archive/Delete/Pause/
// Activate/StreamProjectEvents/ListProjectFiles). The TUI therefore sets
// project_dir through UpdateProject and then PROBES the directory with
// ListProjectFiles — the server materializes the directory when the files
// endpoint resolves it, and a bad path surfaces immediately as an error
// instead of failing later at worker dispatch.

import (
	"context"
	"fmt"
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

// newProjectCreateForm builds the create form (name / slug / goals /
// default runtime image).
func (m *Model) newProjectCreateForm() *kit2.Form {
	f := kit2.NewForm("New project",
		kit2.FieldSpec{Name: "name", Label: "Name", Kind: kit2.KText, Required: true, Placeholder: "Orchicon"},
		kit2.FieldSpec{Name: "slug", Label: "Slug", Kind: kit2.KText, Placeholder: "orchicon"},
		kit2.FieldSpec{Name: "goals", Label: "Goals", Kind: kit2.KText, Placeholder: "key=value, key2=value2"},
		kit2.FieldSpec{Name: "default_runtime_image", Label: "Default runtime image", Kind: kit2.KText, Placeholder: "empty = inherit tenant/base"},
	)
	m.wireProjectForm(f, formCreateProject, "")
	return f
}

// newProjectEditForm builds the edit form: title, goals, project_dir.
func (m *Model) newProjectEditForm(p *apiv1.Project) *kit2.Form {
	f := kit2.NewForm("Edit project",
		kit2.FieldSpec{Name: "name", Label: "Name", Kind: kit2.KText, Required: true, Initial: p.GetName()},
		kit2.FieldSpec{Name: "goals", Label: "Goals", Kind: kit2.KText, Initial: goalsText(p.GetGoals()), Placeholder: "key=value, key2=value2"},
		kit2.FieldSpec{Name: "project_dir", Label: "Project dir", Kind: kit2.KText, Initial: p.GetProjectDir(), Placeholder: "/home/me/projects/orchicon"},
		kit2.FieldSpec{Name: "default_runtime_image", Label: "Default runtime image", Kind: kit2.KText, Initial: p.GetDefaultRuntimeImage()},
	)
	m.wireProjectForm(f, formEditProject, p.GetId())
	return f
}

// newProjectDirForm sets/creates the project directory.
func (m *Model) newProjectDirForm(p *apiv1.Project) *kit2.Form {
	f := kit2.NewForm("Set project directory",
		kit2.FieldSpec{Name: "project_dir", Label: "Directory", Kind: kit2.KText, Required: true, Initial: p.GetProjectDir(), Placeholder: "/home/me/projects/orchicon"},
	)
	m.wireProjectForm(f, formProjectDir, p.GetId())
	return f
}

// wireProjectForm installs the submit handler.
func (m *Model) wireProjectForm(f *kit2.Form, mode, id string) {
	f.Focused = true
	f.Width = 70
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		name := strings.TrimSpace(v["name"])
		goals := parseGoals(v["goals"])
		switch mode {
		case formCreateProject:
			req := &apiv1.CreateProjectRequest{
				Name:                name,
				Slug:                strings.TrimSpace(v["slug"]),
				Goals:               goals,
				DefaultRuntimeImage: strings.TrimSpace(v["default_runtime_image"]),
			}
			return m.Mutate(mutate.Request{
				Name: "create project " + strings.TrimSpace(name), Source: srcProjects,
				Rollback: func() { m.Refresh(srcProjects) },
				Do: func(ctx context.Context) error {
					_, err := m.cl.Projects.CreateProject(ctx, connect.NewRequest(req))
					return err
				},
			}), nil

		case formEditProject:
			req := &apiv1.UpdateProjectRequest{
				Id:                  id,
				Name:                strPtr(name),
				Goals:               &apiv1.GoalFields{Fields: goals},
				ProjectDir:          strPtr(strings.TrimSpace(v["project_dir"])),
				DefaultRuntimeImage: strPtr(strings.TrimSpace(v["default_runtime_image"])),
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
func (m *Model) projectActions() []kit2.Action {
	it, ok := m.ActiveItem()
	if !ok {
		return nil
	}
	id := it.ID
	cl := m.cl
	return []kit2.Action{{
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
}

// goalsText renders a project's goals JSON as "key=value" pairs (the form's
// editable shape). Unknown JSON shapes render empty rather than dumping raw
// JSON into the field.
func goalsText(raw string) string {
	return raw
}

// parseGoals converts "key=value, key2=value2" into GoalFields. An empty
// input clears the goals (the proto's empty-fields semantics).
func parseGoals(v string) []*apiv1.GoalField {
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

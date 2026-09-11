package work

// images.go — Runtime Images: create the spec (CreateRuntimeImage), edit it
// (UpdateRuntimeImage, optimistic concurrency via the version column),
// BUILD it (BuildRuntimeImage — a server-stream whose log chunks render
// live through the kit2 Stream widget) and delete it (DeleteRuntimeImage,
// Confirm-gated).
//
// A ready image must be rebuilt through BuildRuntimeImage, not edited — the
// form is offered for draft/failed specs (the server rejects the rest), and
// the build-status transitions (draft → building → ready|failed) are
// reflected in the row's state pill, the detail pane and the log stream.

import (
	"context"
	"io"
	"strings"
	"time"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/mutate"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// image form modes.
const (
	formCreateImage = "image-create"
	formEditImage   = "image-edit"
)

type imageFormMsg struct {
	mode  string
	image *apiv1.RuntimeImage
	err   error
}

// prepEditImage fetches the selected image spec so the edit form is
// prefilled (and so the submit can carry its version).
func (m *Model) prepEditImage() tea.Cmd {
	it, ok := m.ActiveItem()
	if !ok {
		return nil
	}
	id := it.ID
	cl := m.cl
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		resp, err := cl.Images.GetRuntimeImage(ctx, connect.NewRequest(&apiv1.GetRuntimeImageRequest{Id: id}))
		if err != nil {
			return imageFormMsg{mode: formEditImage, err: err}
		}
		return imageFormMsg{mode: formEditImage, image: resp.Msg.GetRuntimeImage()}
	}
}

// newImageCreateForm builds the spec form (apt packages / toolchains / env
// as JSON; the Dockerfile override replaces the generated one).
func (m *Model) newImageCreateForm() *kit2.Form {
	f := kit2.NewForm("New runtime image",
		kit2.FieldSpec{Name: "name", Label: "Name", Kind: kit2.KText, Required: true, Placeholder: "Go + Node toolchain"},
		kit2.FieldSpec{Name: "slug", Label: "Slug", Kind: kit2.KText, Placeholder: "orchicon-runtime-go-node"},
		kit2.FieldSpec{Name: "description", Label: "Description", Kind: kit2.KTextArea},
		kit2.FieldSpec{Name: "apt_packages", Label: "Apt packages", Kind: kit2.KJSON, Placeholder: `["libgl1","git-lfs"]`, Validate: validateJSON},
		kit2.FieldSpec{Name: "toolchains", Label: "Toolchains", Kind: kit2.KJSON, Placeholder: `["mise install go@1.23"]`, Validate: validateJSON},
		kit2.FieldSpec{Name: "env", Label: "Env", Kind: kit2.KJSON, Placeholder: `{"GOFLAGS":"-mod=mod"}`, Validate: validateJSON},
		kit2.FieldSpec{Name: "dockerfile_override", Label: "Dockerfile override", Kind: kit2.KTextArea, Placeholder: "empty = generate from the fields above"},
		kit2.FieldSpec{Name: "tag", Label: "Tag", Kind: kit2.KText, Placeholder: "empty = <slug>:latest"},
	)
	m.wireImageForm(f, formCreateImage, nil)
	return f
}

// newImageEditForm prefills the spec form from the real image.
func (m *Model) newImageEditForm(img *apiv1.RuntimeImage) *kit2.Form {
	f := kit2.NewForm("Edit runtime image",
		kit2.FieldSpec{Name: "name", Label: "Name", Kind: kit2.KText, Required: true, Initial: img.GetName()},
		kit2.FieldSpec{Name: "slug", Label: "Slug", Kind: kit2.KText, Initial: img.GetSlug()},
		kit2.FieldSpec{Name: "description", Label: "Description", Kind: kit2.KTextArea, Initial: img.GetDescription()},
		kit2.FieldSpec{Name: "apt_packages", Label: "Apt packages", Kind: kit2.KJSON, Initial: img.GetAptPackages(), Validate: validateJSON},
		kit2.FieldSpec{Name: "toolchains", Label: "Toolchains", Kind: kit2.KJSON, Initial: img.GetToolchains(), Validate: validateJSON},
		kit2.FieldSpec{Name: "env", Label: "Env", Kind: kit2.KJSON, Initial: img.GetEnv(), Validate: validateJSON},
		kit2.FieldSpec{Name: "dockerfile_override", Label: "Dockerfile override", Kind: kit2.KTextArea, Initial: img.GetDockerfileOverride()},
		kit2.FieldSpec{Name: "tag", Label: "Tag", Kind: kit2.KText, Initial: img.GetTag()},
	)
	m.wireImageForm(f, formEditImage, img)
	return f
}

// wireImageForm installs the submit handler.
func (m *Model) wireImageForm(f *kit2.Form, mode string, cur *apiv1.RuntimeImage) {
	f.Focused = true
	f.Width = 70
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		switch mode {
		case formCreateImage:
			req := &apiv1.CreateRuntimeImageRequest{
				Name:               strings.TrimSpace(v["name"]),
				Slug:               strings.TrimSpace(v["slug"]),
				Description:        v["description"],
				AptPackages:        strings.TrimSpace(v["apt_packages"]),
				Toolchains:         strings.TrimSpace(v["toolchains"]),
				Env:                strings.TrimSpace(v["env"]),
				DockerfileOverride: v["dockerfile_override"],
				Tag:                strings.TrimSpace(v["tag"]),
			}
			return m.Mutate(mutate.Request{
				Name: "create runtime image " + req.GetName(), Source: srcImages,
				Rollback: func() { m.Refresh(srcImages) },
				Do: func(ctx context.Context) error {
					_, err := m.cl.Images.CreateRuntimeImage(ctx, connect.NewRequest(req))
					return err
				},
			}), nil

		case formEditImage:
			version := int32(0)
			id := ""
			if cur != nil {
				version, id = cur.GetVersion(), cur.GetId()
			}
			req := &apiv1.UpdateRuntimeImageRequest{
				Id:                 id,
				Version:            version,
				Name:               strPtr(strings.TrimSpace(v["name"])),
				Slug:               strPtr(strings.TrimSpace(v["slug"])),
				Description:        strPtr(v["description"]),
				AptPackages:        strPtr(strings.TrimSpace(v["apt_packages"])),
				Toolchains:         strPtr(strings.TrimSpace(v["toolchains"])),
				Env:                strPtr(strings.TrimSpace(v["env"])),
				DockerfileOverride: strPtr(v["dockerfile_override"]),
				Tag:                strPtr(strings.TrimSpace(v["tag"])),
			}
			return m.Mutate(mutate.Request{
				Name: "save runtime image " + req.GetName(), Source: srcImages,
				Rollback: func() { m.Refresh(srcImages) },
				Do: func(ctx context.Context) error {
					_, err := m.cl.Images.UpdateRuntimeImage(ctx, connect.NewRequest(req))
					return err
				},
			}), nil
		}
		return nil, nil
	}
}

// ---------------- build (the live log stream) ----------------

// buildOpenMsg lands the opened build stream (or its failure).
type buildOpenMsg struct {
	stream *connect.ServerStreamForClient[apiv1.BuildRuntimeImageResponse]
	id     string
	tag    string
	cancel context.CancelFunc
	err    error
}

// buildChunkMsg is one streamed build-log chunk.
type buildChunkMsg struct {
	chunk *apiv1.BuildRuntimeImageResponse
	err   error
}

// startBuild opens the daemon build stream (draft/failed → building) after
// reading the spec's current version for optimistic concurrency.
func (m *Model) startBuild() tea.Cmd {
	it, ok := m.ActiveItem()
	if !ok {
		return nil
	}
	id := it.ID
	cl := m.cl
	return func() tea.Msg {
		// The stream outlives a unary deadline: its own context is canceled
		// only when the build finishes (or the screen tears it down).
		ctx, cancel := context.WithCancel(context.Background())
		resp, err := cl.Images.GetRuntimeImage(ctx, connect.NewRequest(&apiv1.GetRuntimeImageRequest{Id: id}))
		if err != nil {
			cancel()
			return buildOpenMsg{id: id, err: err}
		}
		img := resp.Msg.GetRuntimeImage()
		st, err := cl.Images.BuildRuntimeImage(ctx, connect.NewRequest(&apiv1.BuildRuntimeImageRequest{
			Id:      id,
			Version: img.GetVersion(),
		}))
		if err != nil {
			cancel()
			return buildOpenMsg{id: id, err: err}
		}
		return buildOpenMsg{stream: st, id: id, tag: img.GetTag(), cancel: cancel}
	}
}

// readBuildChunk reads ONE streamed chunk (the update loop re-arms it), so
// the build never blocks the UI thread.
func (m *Model) readBuildChunk() tea.Cmd {
	st := m.buildStream
	return func() tea.Msg {
		if st == nil {
			return buildChunkMsg{err: io.EOF}
		}
		if !st.Receive() {
			return buildChunkMsg{err: st.Err()}
		}
		return buildChunkMsg{chunk: st.Msg()}
	}
}

// imageActions is the Runtime Images pane's entity-bound action set.
func (m *Model) imageActions() []kit2.Action {
	it, ok := m.ActiveItem()
	if !ok {
		return nil
	}
	id, title := it.ID, it.Title
	return []kit2.Action{{
		Label: "delete", Key: "x", Danger: true, Source: srcImages,
		Confirm:  "Delete " + title + "?\nThe spec row AND the local docker image are removed (best-effort, refused while a run references the tag).",
		Apply:    func() { m.RemoveRow(srcImages, id) },
		Rollback: func() { m.Refresh(srcImages) },
		Do: func(ctx context.Context) error {
			_, err := m.cl.Images.DeleteRuntimeImage(ctx, connect.NewRequest(&apiv1.DeleteRuntimeImageRequest{Id: id}))
			return err
		},
	}}
}

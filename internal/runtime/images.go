package runtime

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// baseImageLabel marks an image as derived from the Orchicon runtime base.
// Docker inherits labels through `FROM`, so any image built on top of the
// base carries it automatically; the container-create gate uses it to
// accept UI-built custom images without a separate registration step.
const baseImageLabel = "org.orchicon.runtime-base"

// slugPattern matches the image slug / tag basename (same rule as project
// slugs: lowercase words separated by single hyphens).
var slugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// imageTagPattern matches a full docker image reference with a tag, e.g.
// "orchicon-runtime-custom-gui:latest" or "ghcr.io/org/name:v1".
var imageTagPattern = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*:[A-Za-z0-9_.-]+$`)

// ImageList is the response to GET /v1/images.
type ImageList struct {
	Default string   `json:"default"`
	Images  []string `json:"images"`
}

// BuildRequest is the body of POST /v1/images/build. `Base` must resolve
// to the daemon's configured base image; the daemon ALWAYS rewrites the
// Dockerfile's FROM line to its own base image and injects the
// runtime-base label, so the base is structurally part of every built
// image regardless of what `Dockerfile` says.
type BuildRequest struct {
	Slug       string `json:"slug"`
	Tag        string `json:"tag"`
	Base       string `json:"base_image_ref"`
	Dockerfile string `json:"dockerfile"`
}

// BuildResponse is the final status of POST /v1/images/build (streamed as
// AgentEvent lines during the build, one final event at the end).
type BuildResponse struct {
	Tag      string `json:"tag"`
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error,omitempty"`
}

// allowedImages returns the base image plus the operator-configured
// allowlist. The base is ALWAYS present — you can add images, never remove
// the base.
func (d *Daemon) allowedImages() []string {
	out := []string{d.Image}
	for _, img := range d.Images {
		if img != "" && img != d.Image {
			out = append(out, img)
		}
	}
	return out
}

// imageAllowed reports whether a requested image may be used for a runtime
// container. Accepted: the base, any operator-allowlisted stock image, or
// any locally-present image carrying the inherited runtime-base label
// (i.e. built on top of the base — the UI build path always produces
// these). An image that is neither allowlisted nor present locally is
// rejected rather than pulled implicitly, keeping the container-create
// surface closed.
func (d *Daemon) imageAllowed(name string) bool {
	if name == "" || name == d.Image {
		return true
	}
	for _, img := range d.Images {
		if name == img {
			return true
		}
	}
	out, err := d.docker("image", "inspect", "--format", `{{index .Config.Labels "`+baseImageLabel+`"}}`, name)
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == "true"
}

// handleImages implements the image routes:
//
//	GET    /v1/images          -> base + allowlist (work-item dropdown)
//	POST   /v1/images          -> build a custom image (streams logs)
//	DELETE /v1/images?ref=...  -> docker rmi a built image
func (d *Daemon) handleImages(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, ImageList{Default: d.Image, Images: d.allowedImages()})
	case http.MethodPost:
		var req BuildRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpError(w, http.StatusBadRequest, "bad request: "+err.Error())
			return
		}
		if err := d.validateBuild(req); err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		d.handleBuild(w, r, req)
	case http.MethodDelete:
		ref := r.URL.Query().Get("ref")
		if ref == "" || !imageTagPattern.MatchString(ref) {
			httpError(w, http.StatusBadRequest, "bad image ref")
			return
		}
		out, err := d.docker("rmi", "-f", ref)
		if err != nil {
			if strings.Contains(out, "No such image") {
				writeJSON(w, http.StatusOK, map[string]string{"status": "gone"})
				return
			}
			httpError(w, http.StatusInternalServerError, "rmi "+ref+": "+err.Error()+" ("+strings.TrimSpace(out)+")")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
	default:
		httpError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// validateBuild enforces the daemon's build policy: a sane slug/tag, the
// base image reference, and a non-empty Dockerfile.
func (d *Daemon) validateBuild(req BuildRequest) error {
	if req.Slug == "" || !slugPattern.MatchString(req.Slug) {
		return fmt.Errorf("invalid slug (use lowercase words separated by hyphens)")
	}
	if req.Tag == "" || !imageTagPattern.MatchString(req.Tag) {
		return fmt.Errorf("invalid image tag %q", req.Tag)
	}
	if req.Base == "" || req.Base != d.Image {
		return fmt.Errorf("base_image_ref must be the runtime base image (%s)", d.Image)
	}
	if strings.TrimSpace(req.Dockerfile) == "" {
		return fmt.Errorf("dockerfile required")
	}
	return nil
}

// handleBuild runs `docker build` for a custom image, streaming log lines
// as AgentEvent {stream,data} NDJSON until the build exits, then a final
// AgentEvent {event:"exit", exit_code}. The Dockerfile's FROM line is
// ALWAYS rewritten to the daemon's base image and the runtime-base label
// injected — the base-inclusion guarantee is enforced here, not documented.
func (d *Daemon) handleBuild(w http.ResponseWriter, r *http.Request, req BuildRequest) {
	df := rewriteDockerfileBase(req.Dockerfile, d.Image)

	// Build context: a temp dir under the daemon's writable area holding
	// just the generated Dockerfile. Nothing else is copied in.
	ctxDir, err := os.MkdirTemp("", "orchicon-build-*")
	if err != nil {
		httpError(w, http.StatusInternalServerError, "build context: "+err.Error())
		return
	}
	defer os.RemoveAll(ctxDir)
	if err := os.WriteFile(filepath.Join(ctxDir, "Dockerfile"), []byte(df), 0o644); err != nil {
		httpError(w, http.StatusInternalServerError, "write dockerfile: "+err.Error())
		return
	}

	cmd := exec.Command(d.DockerBin, "build", "-t", req.Tag, ctxDir)
	cmd.Dir = ctxDir
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := cmd.Start(); err != nil {
		httpError(w, http.StatusInternalServerError, "docker build: "+err.Error())
		return
	}
	go func() {
		<-r.Context().Done()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()

	w.Header().Set("Content-Type", "application/x-ndjson")
	enc := json.NewEncoder(w)
	flusher, _ := w.(http.Flusher)
	relay := func(stream string, rdr interface{ Read([]byte) (int, error) }) {
		sc := bufio.NewScanner(rdr)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			_ = enc.Encode(AgentEvent{Stream: stream, Data: sc.Text()})
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
	go relay("stdout", stdout)
	go relay("stderr", stderr)

	err = cmd.Wait()
	code := 0
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	if err != nil {
		code = 1
	}
	ev := AgentEvent{Event: "exit", ExitCode: code}
	if err != nil {
		ev.Error = err.Error()
	}
	_ = enc.Encode(ev)
	if flusher != nil {
		flusher.Flush()
	}
	if code == 0 {
		// Rebuilding a tag orphans the previous image (which is still
		// referenced by nothing but its now-overwritten tag). Prune the
		// dangling orphans so repeated custom-image builds don't pile up
		// tens of GB of unreferenced layers. Only untagged images are
		// removed; running containers pin their image by ID.
		if out, perr := d.docker("image", "prune", "-f", "--filter", "dangling=true"); perr == nil {
			_ = out
		}
	}
}

// rewriteDockerfileBase strips any FROM lines from the user's Dockerfile
// and prepends `FROM <base>` + the runtime-base label, guaranteeing every
// built image derives from the base regardless of the author's text.
func rewriteDockerfileBase(dockerfile, base string) string {
	var body []string
	for _, line := range strings.Split(dockerfile, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToUpper(t), "FROM ") {
			continue // the daemon owns the FROM line
		}
		body = append(body, line)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "FROM %s\n", base)
	fmt.Fprintf(&sb, "LABEL %s=true\n", baseImageLabel)
	sb.WriteString(strings.Join(body, "\n"))
	return sb.String()
}

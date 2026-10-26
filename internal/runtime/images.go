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
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/procwin"
)

// baseImageLabel marks an image as derived from the Orchicon runtime base.
// Both engines inherit labels through `FROM`, so any image built on top of the
// base carries it automatically; the container-create gate uses it to
// accept UI-built custom images without a separate registration step.
const baseImageLabel = "org.orchicon.runtime-base"

// slugPattern matches the image slug / tag basename (same rule as project
// slugs: lowercase words separated by single hyphens).
var slugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// imageTagPattern matches a full container image reference with a tag, e.g.
// "orchicon-runtime-custom-gui:latest" or "ghcr.io/org/name:v1".
var imageTagPattern = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*:[A-Za-z0-9_.-]+$`)

// ImageList is the response to GET /v1/images.
type ImageList struct {
	Default string   `json:"default"`
	Images  []string `json:"images"`
	// Infos carries per-image version labels, aligned with Images by index:
	// RuntimeVersion = org.orchicon.runtime.version (stock builds:
	// "<app-version>-<dockerfile-sha12>"), SpecVersion =
	// org.orchicon.runtime.spec-version (custom/daemon builds: the
	// runtime_images.version the image was built from). Either may be empty
	// when the label is absent. The control plane uses this to reconcile
	// canned stock rows against the actual local images (skip auto-builds
	// when container.sh already produced a current pristine image).
	Infos []ImageInfo `json:"infos"`
}

// ImageInfo is the version-label view of one image in ImageList.Infos.
type ImageInfo struct {
	Ref            string `json:"ref"`
	RuntimeVersion string `json:"runtime_version,omitempty"`
	SpecVersion    string `json:"spec_version,omitempty"`
	// Present reports whether the image exists LOCALLY. It is reported
	// explicitly (from the inspect's error) rather than inferred from an
	// empty label: an image built locally without the version label is
	// present-but-unlabelled, and conflating that with "not downloaded" is
	// exactly the ambiguity that made the stock seeder auto-build images
	// nobody asked for. A missing image is NOT an error here.
	Present bool `json:"present"`
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
	// SpecVersion is the runtime_images.version of the spec being built
	// (0 when unknown). It is baked into the image as the
	// org.orchicon.runtime.spec-version label so the row and the image can
	// be cross-checked after the fact; it is the rebuild signal on the
	// service side (built_version == version), not a daemon gate.
	SpecVersion int `json:"spec_version"`
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
	out, err := d.engine("image", "inspect", "--format", d.Engine.ImageLabelFormat(baseImageLabel), name)
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == "true"
}

// imageLabel reads one config label from a local image. Best-effort: an
// inspect failure (image missing) yields "". The image must already be local
// for the label to exist — allowedImages only lists the base + configured
// variants, which scripts/runtime.sh / the release workflow build or pull first.
func (d *Daemon) imageLabel(name, label string) string {
	out, err := d.engine("image", "inspect", "--format", d.Engine.ImageLabelFormat(label), name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// imageReport is the single inspect behind presence + both version labels.
//
// Present comes from the inspect ERROR (a missing image makes inspect fail),
// the labels from one template. A missing image must report Present=false
// with empty labels and NO error: the stock seeder depends on that to tell
// "not downloaded" apart from "needs a rebuild".
func (d *Daemon) imageReport(ref string) ImageInfo {
	info := ImageInfo{Ref: ref}
	// The label template comes from the ENGINE: docker reports image labels at
	// .Config.Labels and podman at .Labels, so a hardcoded template reads ""
	// under the other engine — which would make the create gate reject a
	// perfectly good stock image. The one template per engine lives in
	// engine.go (Engine.ImageLabelFormat); compose it here, once per label.
	format := d.Engine.ImageLabelFormat("org.orchicon.runtime.version") + "|" +
		d.Engine.ImageLabelFormat("org.orchicon.runtime.spec-version")
	out, err := d.engine("image", "inspect", "--format", format, ref)
	if err != nil {
		return info
	}
	info.Present = true
	parts := strings.SplitN(out, "|", 2)
	info.RuntimeVersion = strings.TrimSpace(parts[0])
	if len(parts) == 2 {
		info.SpecVersion = strings.TrimSpace(parts[1])
	}
	return info
}

// handleImages implements the image routes:
//
//	GET    /v1/images          -> base + allowlist (work-item dropdown)
//	POST   /v1/images          -> build a custom image (streams logs)
//	DELETE /v1/images?ref=...  -> engine rmi a built image
func (d *Daemon) handleImages(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		imgs := d.allowedImages()
		infos := make([]ImageInfo, 0, len(imgs))
		for _, ref := range imgs {
			infos = append(infos, d.imageReport(ref))
		}
		writeJSON(w, http.StatusOK, ImageList{Default: d.Image, Images: imgs, Infos: infos})
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
		out, err := d.engine("rmi", "-f", ref)
		if err != nil {
			if d.Engine.IsAbsentOutput(out) {
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

// handleBuild runs an engine `build` for a custom image, streaming log lines
// as AgentEvent {stream,data} NDJSON until the build exits, then a final
// AgentEvent {event:"exit", exit_code}. The Dockerfile's FROM line is
// ALWAYS rewritten to the daemon's base image and the runtime-base label
// injected — the base-inclusion guarantee is enforced here, not documented.
func (d *Daemon) handleBuild(w http.ResponseWriter, r *http.Request, req BuildRequest) {
	w.Header().Set("X-Accel-Buffering", "no")
	df := rewriteDockerfileForBuild(req.Dockerfile, d.Image, req.Tag, req.SpecVersion)

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

	cmd := procwin.Command(d.engineBin(), "build", "-t", req.Tag, ctxDir)
	cmd.Dir = ctxDir
	code, started := d.streamCommand(w, r, req.Tag, "build", cmd)
	if !started || code != 0 {
		return
	}
	// Rebuilding a tag orphans the previous image (which is still
	// referenced by nothing but its now-overwritten tag). Prune the
	// dangling orphans so repeated custom-image builds don't pile up
	// tens of GB of unreferenced layers. Only untagged images are
	// removed; running containers pin their image by ID. Podman accepts
	// the same flags, so the prune is engine-independent.
	if out, perr := d.engine("image", "prune", "-f", "--filter", "dangling=true"); perr == nil {
		_ = out
	}
}

// streamCommand runs one engine command, relaying its stdout/stderr as
// AgentEvent {stream,data} NDJSON until it exits, then a final AgentEvent
// {event:"exit", exit_code}. It is shared by `build` and `pull` so the log
// relay, the cancel registration and the exit framing have exactly ONE
// implementation for both engines and both verbs.
//
// `key` is the active-builds key (the image ref). DELETE
// /v1/images/build?tag=<key> therefore cancels whatever that ref is doing —
// a build or a pull — with no extra endpoint, and GET on the same route is
// the probe. `verb` names the operation in operator-facing errors.
//
// Returns the exit code and whether the command actually started (false
// means an error response was already written).
func (d *Daemon) streamCommand(w http.ResponseWriter, r *http.Request, key, verb string, cmd *exec.Cmd) (code int, started bool) {
	// The engine's output is read through pipes this function owns rather
	// than exec's StdoutPipe/StderrPipe: Wait() closes StdoutPipe descriptors
	// as soon as the child exits, which truncates whatever the relay had not
	// read yet — the exit event could then overtake the last lines of the
	// build log. Owning the read end lets the relays drain to EOF before the
	// terminal event is written.
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return 1, false
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdoutR.Close()
		stdoutW.Close()
		httpError(w, http.StatusInternalServerError, err.Error())
		return 1, false
	}
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	// Own process group: Podman (like Docker) runs the real builder as a
	// child of the CLI, so cancelling must signal the tree, not the CLI.
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		stdoutR.Close()
		stdoutW.Close()
		stderrR.Close()
		stderrW.Close()
		httpError(w, http.StatusInternalServerError, d.Engine.EngineNameForError()+" "+verb+": "+err.Error())
		return 1, false
	}
	// The child owns the write ends now; holding them here would keep the
	// readers from ever seeing EOF.
	stdoutW.Close()
	stderrW.Close()
	// Register the in-flight command for the cancel probe.
	d.buildMu.Lock()
	if d.activeBuilds == nil {
		d.activeBuilds = make(map[string]*exec.Cmd)
	}
	d.activeBuilds[key] = cmd
	d.buildMu.Unlock()
	defer func() {
		d.buildMu.Lock()
		delete(d.activeBuilds, key)
		d.buildMu.Unlock()
	}()
	go func() {
		<-r.Context().Done()
		// Give the engine a moment to finish and flush the exit event before killing.
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-timer.C:
			_ = killGroup(cmd)
		}
	}()

	w.Header().Set("Content-Type", "application/x-ndjson")
	enc := json.NewEncoder(w)
	flusher, _ := w.(http.Flusher)
	// THE TWO RELAYS SHARE ONE ResponseWriter, AND A ResponseWriter IS NOT SAFE FOR CONCURRENT USE.
	//
	// stdout and stderr are drained by two goroutines (they must be, or a build that fills one pipe's
	// buffer while nobody reads the other deadlocks). Both then wrote to `w` through the same encoder
	// with no coordination, which is a DATA RACE on the response — `go test -race` reports it at the
	// Write below, and its consequence in production is exactly what an operator would blame on the
	// build: interleaved or LOST log lines, because two goroutines mutate the writer's buffer at once.
	//
	// A mutex serialises the writes. Each event is one Encode + one Flush under the lock, so a line can
	// never be torn in half and the flush cannot race the next write's header.
	var writeMu sync.Mutex
	writeEvent := func(ev AgentEvent) {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = enc.Encode(ev)
		if flusher != nil {
			flusher.Flush()
		}
	}
	relay := func(stream string, rdr interface{ Read([]byte) (int, error) }) {
		sc := bufio.NewScanner(rdr)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			writeEvent(AgentEvent{Stream: stream, Data: sc.Text()})
		}
	}
	var relayWG sync.WaitGroup
	relayWG.Add(2)
	go func() { defer relayWG.Done(); relay("stdout", stdoutR) }()
	go func() { defer relayWG.Done(); relay("stderr", stderrR) }()

	werr := cmd.Wait()
	code = 0
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	if werr != nil {
		code = 1
	}
	// Drain the engine's remaining output BEFORE the exit event: the terminal
	// event must be the last line the UI sees, otherwise a build log loses its
	// tail (the failure text) exactly when it matters. Bounded, because a
	// grandchild builder can hold the write end open past the CLI's exit.
	drained := make(chan struct{})
	go func() { relayWG.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		stdoutR.Close()
		stderrR.Close()
		<-drained
	}
	stdoutR.Close()
	stderrR.Close()
	ev := AgentEvent{Event: "exit", ExitCode: code}
	if werr != nil {
		ev.Error = werr.Error()
	}
	// Through the same lock: the terminal event must not race a relay that is still draining.
	writeEvent(ev)
	return code, true
}

// handlePull implements POST /v1/images/pull?ref=... — the on-demand FETCH
// for a runtime image that is not downloaded yet.
//
// Fetch is distinct from Deploy: Deploy BUILDS a spec from a Dockerfile,
// Fetch DOWNLOADS a published image. Both stream through the same relay, and
// both are idempotent — an image that is already present short-circuits with
// an immediate exit 0 and no engine command at all.
//
// There is no prune after a pull: nothing is orphaned by downloading.
func (d *Daemon) handlePull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ref := r.URL.Query().Get("ref")
	if ref == "" || !imageTagPattern.MatchString(ref) {
		httpError(w, http.StatusBadRequest, "bad image ref")
		return
	}
	if info := d.imageReport(ref); info.Present {
		w.Header().Set("Content-Type", "application/x-ndjson")
		enc := json.NewEncoder(w)
		_ = enc.Encode(AgentEvent{Stream: "stdout", Data: ref + " is already present locally — nothing to fetch"})
		_ = enc.Encode(AgentEvent{Event: "exit", ExitCode: 0, Skipped: true})
		return
	}
	cmd := procwin.Command(d.engineBin(), "pull", ref)
	d.streamCommand(w, r, ref, "pull", cmd)
}

// rewriteDockerfileBase strips any FROM lines from the user's Dockerfile
// and prepends `FROM <base>` + the runtime-base label, guaranteeing every
// built image derives from the base regardless of the author's text. When
// specVersion > 0 it also injects the org.orchicon.runtime.spec-version
// label next to the runtime-base label so the built image records which
// spec version it was built from (audit/trace; the label is inherited
// through FROM like the base label).

// handleBuildCancel implements cancel/probe for in-flight builds:
//
//	DELETE /v1/images/build?tag=<tag> -> cancel (kills the engine build)
//	GET    /v1/images/build?tag=<tag> -> probe (building/not-building)
func (d *Daemon) handleBuildCancel(w http.ResponseWriter, r *http.Request) {
	tag := r.URL.Query().Get("tag")
	if tag == "" {
		httpError(w, http.StatusBadRequest, "tag required")
		return
	}
	switch r.Method {
	case http.MethodGet:
		d.buildMu.Lock()
		_, building := d.activeBuilds[tag]
		d.buildMu.Unlock()
		if building {
			writeJSON(w, http.StatusOK, map[string]string{"status": "building"})
		} else {
			writeJSON(w, http.StatusOK, map[string]string{"status": "idle"})
		}
	case http.MethodDelete:
		d.buildMu.Lock()
		cmd, ok := d.activeBuilds[tag]
		d.buildMu.Unlock()
		if !ok {
			writeJSON(w, http.StatusOK, map[string]string{"status": "not-building"})
			return
		}
		if cmd.Process != nil {
			_ = killGroup(cmd)
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
	default:
		httpError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (d *Daemon) handleImageInspect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ref := r.URL.Query().Get("ref")
	if ref == "" || !imageTagPattern.MatchString(ref) {
		httpError(w, http.StatusBadRequest, "bad image ref")
		return
	}
	out, err := d.engine("image", "inspect", "--format", d.Engine.ImageLabelFormat("org.orchicon.runtime.spec-version"), ref)
	if err != nil {
		writeJSON(w, http.StatusOK, ImageInspect{Exists: false})
		return
	}
	// engine inspect succeeded => image exists; empty string means no label
	writeJSON(w, http.StatusOK, ImageInspect{Exists: true, SpecVersion: strings.TrimSpace(out)})
}

// rewriteDockerfileBase forces the image to derive from the runtime base, so the base-inclusion
// guarantee holds for every custom image.
//
// SELF-REFERENCE IS THE ONE CASE IT MUST NOT REWRITE. When the caller is building the BASE ITSELF
// (req.Base == req.Tag), prepending `FROM <base>` produces `FROM orchicon-runtime:local` inside the
// build of orchicon-runtime:local — a build that depends on its own output. On a host that has never
// built it, that is a pull of a short name, and with no containers-registries.conf it fails as:
//
//	creating build container: short-name "orchicon-runtime:local" did not resolve to an alias
//
// which is what the operator saw when "deploying the base image". The BASE is the one image that
// derives from the DISTRIBUTION base recorded in its own Dockerfile, so it keeps that FROM.
func rewriteDockerfileBase(dockerfile, base string, specVersion int) string {
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
	if specVersion > 0 {
		fmt.Fprintf(&sb, "LABEL org.orchicon.runtime.spec-version=%d\n", specVersion)
	}
	sb.WriteString(strings.Join(body, "\n"))
	return sb.String()
}

// rewriteDockerfileForBuild is rewriteDockerfileBase with the self-reference case handled: when the
// image being built IS the base, the Dockerfile keeps its own FROM (and gets the labels).
func rewriteDockerfileForBuild(dockerfile, base, tag string, specVersion int) string {
	if !strings.EqualFold(strings.TrimSpace(base), strings.TrimSpace(tag)) {
		return rewriteDockerfileBase(dockerfile, base, specVersion)
	}
	// The base builds from ITS OWN FROM, with the same labels the rewrite would have added.
	var sb strings.Builder
	sb.WriteString(strings.TrimSpace(dockerfile))
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "LABEL %s=true\n", baseImageLabel)
	if specVersion > 0 {
		fmt.Fprintf(&sb, "LABEL org.orchicon.runtime.spec-version=%d\n", specVersion)
	}
	return sb.String()
}

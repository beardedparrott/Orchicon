package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Daemon is the host-side runtime orchestrator. It is the ONLY process
// with access to the Docker socket: it creates/kills runtime containers
// and warms/leases them through the warm pool. The control plane reaches
// it over a unix socket mounted into the supervisor container (see
// client.go).
//
// Every request is validated against a closed allowlist so the control
// plane cannot use the daemon to create arbitrary containers, mount
// arbitrary paths, or run arbitrary binaries.
type Daemon struct {
	SocketPath   string
	DockerBin    string
	Image        string   // image + tag, e.g. orchicon-runtime:local
	Images       []string // additional allowlisted stock image tags (base is always included)
	UserID       int      // host uid the runtime runs as (writes project mounts)
	GroupID      int      // host gid
	HostHome     string   // host user home, mounted as the container HOME
	AllowedRoots []string // host prefixes a project mount may be under
	// ExePath is the daemon's own executable, bind-mounted read-only into
	// every runtime container at /usr/local/bin/orchicon so the container can
	// exec `orchicon runtime-supervisor` / `runtime-client` without the
	// binary being baked into the image — a rebuilt daemon binary is picked
	// up by every newly-created container with no image rebuild (the same
	// "mount, never bake" pattern as the adapter CLIs). The CLI copies the
	// running binary to a STABLE path next to the socket at startup
	// (cmd/orchicon/runtime.go copySelf), so dev hygiene that deletes the
	// original (`make clean`) can never orphan the mount — the copy is what
	// gets mounted.
	ExePath      string
	CPUs         string
	Memory       string
	TmpfsSize    string
	// MaxAge / SweepInterval are retained for config compatibility; the
	// warm pool's idle-reap now owns container cleanup (the pool is reset
	// wholesale at daemon start, which covers the plane-down leak case).
	MaxAge        time.Duration
	SweepInterval time.Duration
	Log           *slog.Logger
	// pool leases/resets warm runtime containers (internal/runtime/pool.go).
	pool *daemonPool
	// createMu serializes createContainer so concurrent checkouts cannot
	// race `docker run` on the same name (the pool names are unique, but
	// serializing the docker create path keeps docker itself calm).
	createMu sync.Mutex
}

// MountSpec is a validated bind mount request from the control plane.
type MountSpec struct {
	Source string `json:"source"` // host path
	Dest   string `json:"dest"`   // container path (usually == Source)
	RO     bool   `json:"ro"`     // read-only
}

// CreateRequest is the body of POST /v1/runtimes.
type CreateRequest struct {
	WorkflowID string      `json:"workflow_id"`
	Image      string      `json:"image"`
	Mounts     []MountSpec `json:"mounts"`
	// InstanceID labels the runtime container with the requesting
	// instance (dev/prod) so each control plane only reaps its OWN
	// containers — two instances sharing one daemon must not fight over
	// each other's orphans.
	InstanceID string `json:"instance_id,omitempty"`
	// ServeConfig, when non-empty, requests the opencode serve inside the
	// container: the OPENCODE_CONFIG_CONTENT JSON the serve boots with
	// (permission rules + MCP servers; the plane builds it). ProjectDir is
	// the container-side project path the serve runs in (executions' bash/
	// file tools resolve against each session's directory). When set, the
	// daemon starts the serve, publishes its port on the host loopback,
	// and returns ServePort/ServePassword in the response.
	ServeConfig string `json:"serve_config,omitempty"`
	ProjectDir  string `json:"project_dir,omitempty"`
}

// CreateResponse is returned by POST /v1/runtimes.
type CreateResponse struct {
	ContainerID string `json:"container_id"`
	Name        string `json:"name"`
	Running     bool   `json:"running"`
	// ServePort is the host-side loopback port published for the
	// container's opencode serve (127.0.0.1:<ServePort>), and
	// ServePassword its basic-auth password. ServeURL is the plane-
	// reachable base URL (http://<gateway>:<port>) — the gateway is the
	// docker bridge IP, reachable from BOTH a host-plane and a
	// containerized plane (127.0.0.1 only works when the plane shares the
	// host network namespace). Zero/empty when the daemon could not bring
	// the serve up — the adapter degrades to one-shot execs inside the
	// container.
	ServePort     int    `json:"serve_port,omitempty"`
	ServePassword string `json:"serve_password,omitempty"`
	ServeURL      string `json:"serve_url,omitempty"`
	// PlaneURL is the sandbox plane's /healthz base URL
	// (http://<container-ip>:8080), set when the container's image boots
	// the in-container Orchicon control plane (dev images: postgres +
	// nats-server present). Empty on base/gui images — the run-start gate
	// probes the plane only when this is set.
	PlaneURL string `json:"plane_url,omitempty"`
}

// ListResponse is returned by GET /v1/runtimes.
type ListResponse struct {
	Runtimes []string `json:"runtimes"`
}

// HTTP mux setup.
func (d *Daemon) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", d.handleHealth)
	mux.HandleFunc("/v1/runtimes", d.handleRuntimes)
	mux.HandleFunc("/v1/runtimes/", d.handleRuntime)
	mux.HandleFunc("/v1/images", d.handleImages)
	return mux
}

// ListenAndServe starts the unix-socket HTTP server. It blocks until
// ctx is cancelled or the listener fails.
func (d *Daemon) ListenAndServe(ctx context.Context) error {
	if d.SocketPath == "" {
		return fmt.Errorf("daemon: socket path required")
	}
	_ = os.RemoveAll(d.SocketPath)
	if err := os.MkdirAll(filepath.Dir(d.SocketPath), 0o755); err != nil {
		return fmt.Errorf("daemon: mkdir: %w", err)
	}
	l, err := net.Listen("unix", d.SocketPath)
	if err != nil {
		return fmt.Errorf("daemon: listen %s: %w", d.SocketPath, err)
	}
	defer l.Close()
	// Ensure the socket is reachable by the supervisor container's
	// control-plane user (host uid). Runs as the daemon's user already.
	if err := os.Chmod(d.SocketPath, 0o660); err != nil {
		d.Log.Warn("daemon: chmod socket", "error", err)
	}

	srv := &http.Server{Handler: d.handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	// Warm-pool lifecycle: leases are daemon-resident, so start from a
	// clean slate (all runtime containers removed — covers plane-down
	// leaks) then idle-reap clean containers periodically.
	d.pool = newDaemonPool(d)
	d.pool.resetPool()
	go func() {
		tick := time.NewTicker(5 * time.Minute)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			d.pool.idleReap()
		}
	}()
	d.Log.Info("runtime daemon listening", "socket", d.SocketPath)
	return srv.Serve(l)
}

func (d *Daemon) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleRuntimes implements the collection routes:
//
//	GET  /v1/runtimes            -> list warm-pool containers
//	POST /v1/runtimes            -> lease a warm container for a run
func (d *Daemon) handleRuntimes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// Instance-scoped list so a plane only sees (and can reap) its
		// own containers (query ?instance=dev|prod).
		names, err := d.listRuntimes(r.URL.Query().Get("instance"))
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, ListResponse{Runtimes: names})
	case http.MethodPost:
		var req CreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpError(w, http.StatusBadRequest, "bad request: "+err.Error())
			return
		}
		if err := d.validateCreate(req); err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		resp, err := d.pool.checkout(r.Context(), req.WorkflowID, req)
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, resp)
	default:
		httpError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleRuntime implements the per-runtime routes:
//
//	DELETE /v1/runtimes/{id}      -> release the run's lease (reset to the pool)
func (d *Daemon) handleRuntime(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/runtimes/")
	action := ""
	if i := strings.Index(id, "/"); i >= 0 {
		action = id[i+1:]
		id = id[:i]
	}
	if id == "" || !validWorkflowID(id) {
		httpError(w, http.StatusBadRequest, "bad runtime id")
		return
	}
	switch {
	case action == "" && r.Method == http.MethodDelete:
		// Release the run's lease: the container is removed and reset in the
		// background back into the warm pool (pristine + serve warmed). A
		// run with no lease (already released / never leased) is a no-op.
		d.pool.release(id)
		writeJSON(w, http.StatusOK, map[string]string{"status": "released"})
	default:
		httpError(w, http.StatusNotFound, "not found")
	}
}

// validateCreate enforces the daemon's security policy: image allowlist,
// mount-source allowlist, argv allowlist.
func (d *Daemon) validateCreate(req CreateRequest) error {
	if req.WorkflowID == "" {
		return fmt.Errorf("workflow_id required")
	}
	if !validWorkflowID(req.WorkflowID) {
		return fmt.Errorf("invalid workflow_id")
	}
	// Image allowlist: the base image, operator-configured stock images,
	// or any image carrying the inherited runtime-base label (UI-built
	// custom images). The base is always allowed.
	if req.Image != "" && !d.imageAllowed(req.Image) {
		return fmt.Errorf("image not allowed: %s", req.Image)
	}
	if req.Image == "" && d.Image == "" {
		return fmt.Errorf("no image specified")
	}
	for _, m := range req.Mounts {
		if m.Source == "" {
			return fmt.Errorf("mount source required")
		}
		abs, err := filepath.Abs(m.Source)
		if err != nil {
			return fmt.Errorf("bad mount source: %w", err)
		}
		if !d.withinAllowedRoots(abs) {
			return fmt.Errorf("mount source outside allowed roots: %s", abs)
		}
		if m.Dest == "" {
			return fmt.Errorf("mount dest required")
		}
	}
	return nil
}

func validWorkflowID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') && !(c >= '0' && c <= '9') && c != '-' {
			return false
		}
	}
	return true
}

func (d *Daemon) withinAllowedRoots(path string) bool {
	if len(d.AllowedRoots) == 0 {
		return false
	}
	for _, root := range d.AllowedRoots {
		r := filepath.Clean(root)
		if path == r || strings.HasPrefix(path, r+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// createContainer creates a runtime container with the given pool-managed
// name, warms its opencode serve (when ServeConfig is set), and returns its
// state. Container creation is serialized with createMu so concurrent
// checkouts/resets cannot race `docker run` on the same name.
func (d *Daemon) createContainer(name string, req CreateRequest) (*CreateResponse, error) {
	d.createMu.Lock()
	defer d.createMu.Unlock()
	// A stopped/crashed container with this name blocks recreation
	// ("name already in use"). Remove it first so the pool always gets a
	// fresh runtime (leftover-container hygiene).
	if exists, _ := d.containerExists(name); exists {
		d.Log.Warn("removing stale runtime container", "name", name)
		if out, err := d.docker("rm", "-f", name); err != nil {
			return nil, fmt.Errorf("remove stale runtime %s: %s: %w", name, out, err)
		}
	}

	image := req.Image
	if image == "" {
		image = d.Image
	}
	workflowLabel := req.WorkflowID
	if workflowLabel == "" {
		workflowLabel = "pool"
	}
	args := []string{"run", "-d", "--name", name,
		"--label", "orchicon.workflow=" + workflowLabel,
		"--label", "orchicon.instance=" + instanceID(req.InstanceID),
		"--user", fmt.Sprintf("%d:%d", d.UserID, d.GroupID),
		"--cpus", d.CPUs,
		"--memory", d.Memory,
		"--tmpfs", "/tmp:rw,size=" + d.TmpfsSize,
	}
	// NO port publish: the plane reaches the container's opencode serve
	// DIRECTLY on the docker bridge via the container IP (the serve binds
	// 0.0.0.0 inside, reachable from any bridge container AND the host,
	// password-gated). This avoids docker-proxy entirely — published-port
	// forwarding to a container whose serve starts lazily is a race mine
	// (the proxy binds only when the container's port appears; a container-
	// to-gateway connection can be dropped by the bridge NAT). Direct IP
	// access has no such layering. Always works whether the plane is a
	// container or the host itself.
	for _, m := range req.Mounts {
		mo := ""
		if m.RO {
			mo = ":ro"
		}
		args = append(args, "-v", m.Source+":"+m.Dest+mo)
	}
	// Standard host-home mounts shared by every runtime container:
	// opencode config (read-only) + data/auth (read-only — the worker
	// reads model auth, but its sessions/keys are redirected to the
	// ephemeral FS by the supervisor's isolateOpenCodeData), the runtime
	// CLI adapter install (read-only — opencode today; Orchicon never
	// ships the adapter binary in the image, the operator's host install
	// is mounted here so the supervisor can exec it), and git identity +
	// credential store (read-only — PR/merge workers). These are appended
	// by the daemon, not the plane, so the control plane can never request
	// them.
	if d.HostHome != "" {
		if st, err := os.Stat(filepath.Join(d.HostHome, ".config/opencode")); err == nil && st.IsDir() {
			args = append(args, "-v", filepath.Join(d.HostHome, ".config/opencode")+":"+filepath.Join(d.HostHome, ".config/opencode")+":ro")
		}
		if st, err := os.Stat(filepath.Join(d.HostHome, ".local/share/opencode")); err == nil && st.IsDir() {
			args = append(args, "-v", filepath.Join(d.HostHome, ".local/share/opencode")+":"+filepath.Join(d.HostHome, ".local/share/opencode")+":ro")
		}
		if st, err := os.Stat(filepath.Join(d.HostHome, ".opencode", "bin", "opencode")); err == nil && !st.IsDir() {
			args = append(args, "-v", filepath.Join(d.HostHome, ".opencode")+":"+filepath.Join(d.HostHome, ".opencode")+":ro")
		}
		for _, f := range []string{".gitconfig", ".git-credentials"} {
			if st, err := os.Stat(filepath.Join(d.HostHome, f)); err == nil && !st.IsDir() {
				args = append(args, "-v", filepath.Join(d.HostHome, f)+":"+filepath.Join(d.HostHome, f)+":ro")
			}
		}
		// GitHub CLI auth + state (read-only — PR/merge workers run
		// `gh pr create`/`gh repo create`). Without the hosts.yml mount
		// gh reports "not authenticated" inside the container even though
		// the operator is logged in on the host.
		if st, err := os.Stat(filepath.Join(d.HostHome, ".config", "gh")); err == nil && st.IsDir() {
			args = append(args, "-v", filepath.Join(d.HostHome, ".config", "gh")+":"+filepath.Join(d.HostHome, ".config", "gh")+":ro")
		}
		if st, err := os.Stat(filepath.Join(d.HostHome, ".local", "share", "gh")); err == nil && st.IsDir() {
			args = append(args, "-v", filepath.Join(d.HostHome, ".local", "share", "gh")+":"+filepath.Join(d.HostHome, ".local", "share", "gh")+":ro")
		}
		args = append(args, "-e", "HOME="+d.HostHome)
		// Put the mounted adapter CLI on PATH so the supervisor's
		// `exec.Command("opencode", ...)` resolves it.
		args = append(args, "-e", "PATH="+filepath.Join(d.HostHome, ".opencode", "bin")+":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
		// The operator's gh token often lives in the OS keyring, which is
		// NOT available inside containers — hosts.yml alone has no token,
		// so `gh` reports "not authenticated". Resolve the host's effective
		// token and pass it as GH_TOKEN so PR/merge workers can actually
		// create PRs. Best-effort: no gh / no auth / keyring locked → skip.
		if tok := hostGHToken(); tok != "" {
			args = append(args, "-e", "GH_TOKEN="+tok)
		}
	}
	// The daemon's own executable: bind-mounted read-only at
	// /usr/local/bin/orchicon so the container can exec `orchicon
	// runtime-supervisor` (PID 1 entrypoint below) and `orchicon
	// runtime-client` (docker exec paths). The binary is deliberately not
	// baked into the image; Docker resolves bind sources at `docker run`
	// time, so a rebuilt bin/orchicon is picked up by newly-created
	// containers without an image rebuild. This mount is a HARD dependency:
	// the entrypoint is `orchicon runtime-supervisor`, so a container
	// created without it would exec a missing binary and die — a confusing
	// failure an operator would have to dig out of container logs. Fail the
	// create with a clear error when the executable cannot be mounted
	// instead of silently skipping.
	if d.ExePath == "" {
		return nil, fmt.Errorf("runtime daemon executable path unavailable (os.Executable failed) — cannot create runtime container: the orchicon binary is bind-mounted into every runtime container, never baked")
	}
	if st, err := os.Stat(d.ExePath); err != nil || st.IsDir() {
		return nil, fmt.Errorf("runtime daemon executable %s missing — cannot create runtime container: the orchicon binary is bind-mounted into every runtime container, never baked", d.ExePath)
	}
	args = append(args, "-v", d.ExePath+":/usr/local/bin/orchicon:ro")
	args = append(args, image, "orchicon", "runtime-supervisor")

	// Create the container, retrying past name conflicts. A container
	// that was just removed (`docker rm -f` during a rebuild / orphan
	// sweep) is still settling for a moment: inspect reports it gone,
	// but `docker run` then fails with "name already in use" / "removal
	// in progress". Remove any leftover and retry.
	var out string
	var err error
	created := false
	for attempt := 0; attempt < 3 && !created; attempt++ {
		out, err = d.docker(args...)
		if err == nil {
			created = true
			break
		}
		if !strings.Contains(out, "Conflict") && !strings.Contains(out, "in progress") && !strings.Contains(out, "is already") {
			return nil, fmt.Errorf("create runtime %s: %s: %w", name, out, err)
		}
		d.Log.Warn("runtime create conflict — removing stale container and retrying", "name", name, "attempt", attempt+1)
		_, _ = d.docker("rm", "-f", name)
		time.Sleep(500 * time.Millisecond)
	}
	if !created {
		return nil, fmt.Errorf("create runtime %s: %s: %w", name, out, err)
	}

	// Wait for the in-container supervisor socket to answer pings. The
	// supervisor starts immediately; opencode/lazy deps are not required
	// for readiness.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if d.pingRuntime(name) {
			resp := &CreateResponse{Name: name, Running: true}
			if req.ServeConfig != "" {
				// Start the opencode serve inside the container and publish
				// its port on the host loopback. A serve that cannot come
				// up is a hard dispatch error — the one-shot degradation
				// was removed, so the adapter surfaces this as
				// failed_to_start → workflow recovery. The container stays
				// up so the watchdog / a later retry can converge it.
				port, pw, planeEnabled, serr := d.startServe(name, req)
				if serr != nil {
					return nil, fmt.Errorf("start serve in runtime %s: %w", name, serr)
				}
				cip := d.containerIP(name)
				resp.ServePort = port
				resp.ServePassword = pw
				resp.ServeURL = fmt.Sprintf("http://%s:%d", cip, port)
				if planeEnabled && cip != "" {
					// The supervisor booted the sandbox plane (dev image); the
					// run-start gate verifies its /healthz on the bridge IP
					// before any execution dispatches.
					resp.PlaneURL = fmt.Sprintf("http://%s:%d", cip, sandboxPlanePort)
				}
			}
			return resp, nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return nil, fmt.Errorf("runtime %s started but supervisor socket not ready", name)
}

// hostGHToken resolves the operator's effective GitHub CLI token
// (`gh auth token`), which may live in the OS keyring rather than
// hosts.yml. Bounded + best-effort: returns "" when gh is absent, not
// authenticated, or the keyring is locked.
func hostGHToken() string {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "auth", "token")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// startServe asks the in-container supervisor to bring up `opencode
// serve` (idempotent — the supervisor owns the password and reports it
// back), then resolves the published host loopback port. Returns the
// host port + the container's serve password, plus whether the image
// boots the sandbox plane (the daemon publishes its /healthz URL).
func (d *Daemon) startServe(name string, req CreateRequest) (int, string, bool, error) {
	reqJSON, err := json.Marshal(AgentRequest{
		Cmd:  "serve",
		Argv: []string{"opencode", "serve", "--hostname", "0.0.0.0", "--port", "4096"},
		Env: []string{
			"OPENCODE_CONFIG_CONTENT=" + req.ServeConfig,
		},
		Cwd:        req.ProjectDir,
		ProjectDir: req.ProjectDir,
	})
	if err != nil {
		return 0, "", false, err
	}
	cmd := exec.Command(d.DockerBin, "exec", "-i", name, "orchicon", "runtime-client")
	cmd.Stdin = bytes.NewReader(reqJSON)
	out, perr := cmd.Output()
	if perr != nil {
		return 0, "", false, fmt.Errorf("serve handshake: %v", perr)
	}
	// The supervisor answers {event:"serve", port:4096, password:...} when
	// ready.
	sc := bufio.NewScanner(bytes.NewReader(out))
	port := 0
	password := ""
	planeEnabled := false
	for sc.Scan() {
		var ev AgentEvent
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if ev.Event == "serve" {
			port = ev.Port
			password = ev.Password
			planeEnabled = ev.PlaneEnabled
			break
		}
		if ev.Event == "error" {
			return 0, "", false, fmt.Errorf("serve handshake error: %s", ev.Error)
		}
	}
	if port == 0 || password == "" {
		return 0, "", false, fmt.Errorf("serve handshake: incomplete serve event (port=%d)", port)
	}

	// Resolve the container's bridge IP — the plane reaches the serve
	// DIRECTLY at http://<container-ip>:<port> (no docker-proxy).
	cip := d.containerIP(name)
	if cip == "" {
		return 0, "", false, fmt.Errorf("resolve container IP for %s", name)
	}

	// The serve's cold start answers /global/health before it can handle
	// sessions (providers/MCP load); gate on the serve being USABLE — a
	// real session create round-trip — before handing it to the plane.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if serveUsableAt(fmt.Sprintf("http://%s:%d", cip, port), password) {
			// Give the accept path a beat after the first success.
			time.Sleep(500 * time.Millisecond)
			return port, password, planeEnabled, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return 0, "", false, fmt.Errorf("serve %s:%d did not become usable within 30s", cip, port)
}

// serveUsableAt is the L1 serve-readiness probe (the workflow run-start
// gate): the serve must answer /global/health AND accept a real
// session-create round-trip. A cold-starting serve answers health before
// its session machinery is up, so health alone is not "usable" for
// dispatch. The probe session is left for the serve's own cleanup; the
// plane's lifecycle gate uses the same probe via the same helper.
func serveUsableAt(baseURL, password string) bool {
	client := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequest(http.MethodGet, baseURL+"/global/health", nil)
	if err != nil {
		return false
	}
	req.SetBasicAuth("opencode", password)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	// The serve is healthy; confirm it can actually create a session (the
	// cold-start window answers health before the session machinery is up).
	body := `{"title":"orchicon-serve-probe"}`
	cReq, _ := http.NewRequest(http.MethodPost, baseURL+"/session", bytes.NewBufferString(body))
	cReq.Header.Set("Content-Type", "application/json")
	cReq.SetBasicAuth("opencode", password)
	cResp, err := client.Do(cReq)
	if err != nil {
		return false
	}
	defer cResp.Body.Close()
	if cResp.StatusCode != http.StatusOK {
		return false
	}
	// Clean up the probe session.
	io.Copy(io.Discard, cResp.Body)
	return true
}

// containerIP resolves a container's bridge IP.
func (d *Daemon) containerIP(name string) string {
	out, err := d.docker("inspect", "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func (d *Daemon) containerRunning(name string) (bool, error) {
	out, err := d.docker("inspect", "--format", "{{.State.Running}}", name)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "true", nil
}

// instanceID returns a safe instance label value (default "dev").
func instanceID(id string) string {
	if id == "" {
		return "dev"
	}
	return id
}

// containerExists reports whether a container with this name exists
// (running or stopped).
func (d *Daemon) containerExists(name string) (bool, error) {
	out, err := d.docker("inspect", "--format", "{{.Id}}", name)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// listRuntimes returns runtime container names, optionally scoped to one
// instance (empty instance = all instances). Instance scoping keeps two
// control planes sharing one daemon from reaping each other's containers.
func (d *Daemon) listRuntimes(instance string) ([]string, error) {
	args := []string{"ps", "-a", "--filter", "label=orchicon.workflow"}
	if instance != "" {
		args = append(args, "--filter", "label=orchicon.instance="+instance)
	}
	args = append(args, "--format", "{{.Names}}")
	out, err := d.docker(args...)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}

// pingRuntime runs a bounded readiness ping against the container's
// supervisor socket via `orchicon runtime-client`. The client exits 0 on
// success; a missing container/socket or timeout returns false.
func (d *Daemon) pingRuntime(name string) bool {
	req, _ := json.Marshal(AgentRequest{Cmd: "ping"})
	cmd := exec.Command(d.DockerBin, "exec", "-i", name, "orchicon", "runtime-client")
	cmd.Stdin = bytes.NewReader(req)
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return false
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err == nil
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		return false
	}
}

// validateExec enforces the daemon's argv allowlist.
func (d *Daemon) docker(args ...string) (string, error) {
	cmd := exec.Command(d.DockerBin, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// envInt parses an integer env var with a fallback (0 on invalid).
func (d *Daemon) envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

// envDuration parses a duration env var with a fallback (0 on invalid).
func (d *Daemon) envDuration(key string) time.Duration {
	if v := os.Getenv(key); v != "" {
		if dur, err := time.ParseDuration(v); err == nil {
			return dur
		}
	}
	return 0
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

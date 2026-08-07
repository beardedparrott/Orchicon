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
	"strings"
	"sync"
	"time"
)

// Daemon is the host-side runtime orchestrator. It is the ONLY process
// with access to the Docker socket: it creates/kills runtime containers
// and brokers execs into them. The control plane reaches it over a unix
// socket mounted into the supervisor container (see client.go).
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
	// ExePath is the daemon's own executable, resolved at process start. It
	// is bind-mounted read-only into every runtime container at
	// /usr/local/bin/orchicon so the container can exec `orchicon
	// runtime-supervisor` / `runtime-client` without the binary being baked
	// into the image — a rebuilt daemon binary is picked up by every
	// newly-created container with no image rebuild (the same "mount, never
	// bake" pattern as the adapter CLIs).
	ExePath      string
	CPUs         string
	Memory       string
	TmpfsSize    string
	// MaxAge is the hard backstop for leaked runtime containers: any
	// orchicon-runtime-* container older than this is removed. The
	// control plane's state-aware sweep is the primary cleanup (it knows
	// which runs are active); this catches the plane-down / crashed case
	// where a container would otherwise linger forever.
	MaxAge time.Duration
	// SweepInterval is how often the age-based orphan sweep runs.
	SweepInterval time.Duration
	Log           *slog.Logger
	// createMu serializes createRuntime so concurrent Create calls for the
	// same workflow (WorkflowReconciler.EnsureForRun + adapter self-heal)
	// cannot race `docker run` on the same container name.
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
}

// ExecRequest is the body of POST /v1/runtimes/{id}/exec.
type ExecRequest struct {
	ExecID     string   `json:"exec_id"`
	Argv       []string `json:"argv"`
	Env        []string `json:"env"`
	Cwd        string   `json:"cwd"`
	ProjectDir string   `json:"project_dir"`
	// ReconnectGraceSeconds is how long the supervisor keeps an orphaned
	// child (no attached client) running before killing it, so the client
	// can re-attach to a transiently broken stream. Zero = default (60).
	ReconnectGraceSeconds int64 `json:"reconnect_grace_seconds,omitempty"`
}

// SignalRequest is the body of POST /v1/runtimes/{id}/signal.
type SignalRequest struct {
	ExecID string `json:"exec_id"`
	Signal string `json:"signal"`
}

// ListResponse is returned by GET /v1/runtimes.
type ListResponse struct {
	Runtimes []string `json:"runtimes"`
}

func (d *Daemon) containerName(workflowID string) string {
	return "orchicon-runtime-" + workflowID
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
	d.Log.Info("runtime daemon listening", "socket", d.SocketPath)
	// Age-based orphan sweep: removes leaked runtime containers whose
	// run is long gone (plane down / crashed) but which no one reaped.
	go d.sweepOrphans(ctx)
	return srv.Serve(l)
}

// sweepOrphans periodically removes runtime containers older than MaxAge.
// It is the hard backstop for leftover containers; the control plane's
// state-aware adopt sweep is the primary (and faster) cleanup. The first
// sweep runs immediately at daemon start so containers leaked by a crash
// are reaped within seconds of the daemon coming back, not after the
// first interval tick.
func (d *Daemon) sweepOrphans(ctx context.Context) {
	if d.MaxAge <= 0 {
		return
	}
	interval := d.SweepInterval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	d.sweepOnce()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		d.sweepOnce()
	}
}

func (d *Daemon) sweepOnce() {
	names, err := d.listRuntimes("")
	if err != nil {
		d.Log.Warn("orphan sweep: list", "error", err)
		return
	}
	for _, name := range names {
		created, err := d.containerCreated(name)
		if err != nil {
			continue
		}
		if time.Since(created) <= d.MaxAge {
			continue
		}
		d.Log.Warn("orphan sweep: removing aged runtime container", "name", name, "age", time.Since(created).Round(time.Minute).String())
		if out, err := d.docker("rm", "-f", name); err != nil {
			d.Log.Warn("orphan sweep: remove failed", "name", name, "error", err, "out", strings.TrimSpace(out))
		}
	}
}

// containerCreated returns the container's creation time (RFC3339 from
// `docker inspect --format {{.Created}}`).
func (d *Daemon) containerCreated(name string) (time.Time, error) {
	out, err := d.docker("inspect", "--format", "{{.Created}}", name)
	if err != nil {
		return time.Time{}, err
	}
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(out))
	if err != nil {
		return time.Time{}, err
	}
	return t, nil
}

func (d *Daemon) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleRuntimes implements the collection routes:
//
//	GET  /v1/runtimes            -> list active runtime containers (adopt)
//	POST /v1/runtimes            -> create a runtime container
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
		resp, err := d.createRuntime(req)
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
//	POST /v1/runtimes/{id}/exec   -> stream an exec into the runtime
//	POST /v1/runtimes/{id}/signal -> signal a running exec
//	DELETE /v1/runtimes/{id}      -> kill + remove the runtime container
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
	case action == "exec" && r.Method == http.MethodPost:
		d.handleExec(w, r, id)
	case action == "signal" && r.Method == http.MethodPost:
		d.handleSignal(w, r, id)
	case strings.HasPrefix(action, "execs/") && r.Method == http.MethodGet:
		execID := strings.TrimPrefix(action, "execs/")
		if execID == "" || strings.Contains(execID, "/") {
			httpError(w, http.StatusBadRequest, "bad exec id")
			return
		}
		d.handleExecStatus(w, id, execID)
	case action == "" && r.Method == http.MethodDelete:
		d.handleKill(w, id)
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

// createRuntime ensures a runtime container exists for the workflow and
// returns its state. Idempotent: if it is already running, returns it.
//
// Container creation is serialized with createMu: the control plane calls
// Create from BOTH the WorkflowReconciler (EnsureForRun when a run leaves
// pending) and the adapter (self-heal before every exec), so two requests
// for the same workflow can race `docker run` on the same name — one wins
// and the other hits "name already in use", removes the winner's container
// mid-setup, and the exec lands on a container that is being recreated.
// The mutex makes createRuntime atomic per workflow.
func (d *Daemon) createRuntime(req CreateRequest) (*CreateResponse, error) {
	name := d.containerName(req.WorkflowID)
	d.createMu.Lock()
	defer d.createMu.Unlock()
	if running, _ := d.containerRunning(name); running {
		resp := &CreateResponse{Name: name, Running: true}
		// Converge the opencode serve on an already-running container: the
		// WorkflowReconciler creates the container at run start WITHOUT a
		// serve config, and the first dispatch (this Create with
		// ServeConfig) brings the serve up. Idempotent (the supervisor
		// owns the password + answers the same port).
		if req.ServeConfig != "" {
			if port, pw, serr := d.startServe(name, req); serr != nil {
				d.Log.Warn("runtime opencode serve unavailable — degrading to one-shot execs",
					"runtime", name, "error", serr)
			} else {
				resp.ServePort = port
				resp.ServePassword = pw
				resp.ServeURL = fmt.Sprintf("http://%s:%d", d.containerIP(name), port)
			}
		}
		return resp, nil
	}
	// A stopped/crashed container with this name blocks recreation
	// ("name already in use"). Remove it first so an active run always
	// gets a fresh runtime (leftover-container hygiene).
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
	args := []string{"run", "-d", "--name", name,
		"--label", "orchicon.workflow=" + req.WorkflowID,
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
		args = append(args, "-e", "HOME="+d.HostHome)
		// Put the mounted adapter CLI on PATH so the supervisor's
		// `exec.Command("opencode", ...)` resolves it.
		args = append(args, "-e", "PATH="+filepath.Join(d.HostHome, ".opencode", "bin")+":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
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
				// up is NOT fatal to the container — the adapter degrades
				// to one-shot execs — but the password is generated fresh
				// per container and the host port is reported back.
				port, pw, serr := d.startServe(name, req)
				if serr != nil {
					d.Log.Warn("runtime opencode serve unavailable — degrading to one-shot execs",
						"runtime", name, "error", serr)
				} else {
					resp.ServePort = port
					resp.ServePassword = pw
					resp.ServeURL = fmt.Sprintf("http://%s:%d", d.containerIP(name), port)
				}
			}
			return resp, nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return nil, fmt.Errorf("runtime %s started but supervisor socket not ready", name)
}

// startServe asks the in-container supervisor to bring up `opencode
// serve` (idempotent — the supervisor owns the password and reports it
// back), then resolves the published host loopback port. Returns the
// host port + the container's serve password.
func (d *Daemon) startServe(name string, req CreateRequest) (int, string, error) {
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
		return 0, "", err
	}
	cmd := exec.Command(d.DockerBin, "exec", "-i", name, "orchicon", "runtime-client")
	cmd.Stdin = bytes.NewReader(reqJSON)
	out, perr := cmd.Output()
	if perr != nil {
		return 0, "", fmt.Errorf("serve handshake: %v", perr)
	}
	// The supervisor answers {event:"serve", port:4096, password:...} when
	// ready.
	sc := bufio.NewScanner(bytes.NewReader(out))
	port := 0
	password := ""
	for sc.Scan() {
		var ev AgentEvent
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if ev.Event == "serve" {
			port = ev.Port
			password = ev.Password
			break
		}
		if ev.Event == "error" {
			return 0, "", fmt.Errorf("serve handshake error: %s", ev.Error)
		}
	}
	if port == 0 || password == "" {
		return 0, "", fmt.Errorf("serve handshake: incomplete serve event (port=%d)", port)
	}

	// Resolve the container's bridge IP — the plane reaches the serve
	// DIRECTLY at http://<container-ip>:<port> (no docker-proxy).
	cip := d.containerIP(name)
	if cip == "" {
		return 0, "", fmt.Errorf("resolve container IP for %s", name)
	}

	// The serve's cold start answers /global/health before it can handle
	// sessions (providers/MCP load); gate on the serve being USABLE — a
	// real session create round-trip — before handing it to the plane.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if d.serveUsable(cip, port, password) {
			// Give the accept path a beat after the first success.
			time.Sleep(500 * time.Millisecond)
			return port, password, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return 0, "", fmt.Errorf("serve %s:%d did not become usable within 30s", cip, port)
}

// serveUsable verifies the container serve answers health AND can create a
// session (a cold-starting serve answers health before it can handle real
// requests). Returns true once usable.
func (d *Daemon) serveUsable(cip string, port int, password string) bool {
	client := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://%s:%d/global/health", cip, port), nil)
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
	cReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://%s:%d/session", cip, port), bytes.NewBufferString(body))
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

// handleExec streams a dispatch into the runtime container: it pipes the
// request JSON into `orchicon runtime-client` (via docker exec) and
// relays the JSON-lines events to the HTTP response, flushing each line.
func (d *Daemon) handleExec(w http.ResponseWriter, r *http.Request, id string) {
	name := d.containerName(id)
	var req ExecRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "bad request: "+err.Error())
		return
	}
	if err := validateExec(req); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}

	reqJSON, err := json.Marshal(AgentRequest{
		Cmd:                   "exec",
		ExecID:                req.ExecID,
		Argv:                  req.Argv,
		Env:                   req.Env,
		Cwd:                   req.Cwd,
		ProjectDir:            req.ProjectDir,
		ReconnectGraceSeconds: req.ReconnectGraceSeconds,
	})
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}

	cmd := exec.Command(d.DockerBin, "exec", "-i", name, "orchicon", "runtime-client")
	cmd.Stdin = bytes.NewReader(reqJSON)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := cmd.Start(); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer cmd.Wait()

	// When the control plane's request ends (client cancel, plane shutdown,
	// or a transient transport break), close the docker-exec CLI. We do NOT
	// SIGKILL the opencode child here: a transient break must not kill a
	// healthy execution. Closing the docker-exec makes the supervisor see
	// the disconnect; it keeps the child running for the reconnect grace
	// (so the client can re-attach) and kills it only if nothing re-attaches
	// within that window. Explicit termination (wall-clock deadline, abort,
	// plane shutdown) signals the child directly via the signal endpoint.
	go func() {
		<-r.Context().Done()
		d.Log.Info("runtime exec request ended — disconnecting stream", "runtime", name, "exec", req.ExecID)
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()

	w.Header().Set("Content-Type", "application/x-ndjson")
	enc := json.NewEncoder(w)
	dec := json.NewDecoder(stdout)
	for {
		var ev AgentEvent
		if err := dec.Decode(&ev); err != nil {
			break
		}
		_ = enc.Encode(ev)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if ev.Event == "exit" || ev.Event == "error" {
			break
		}
	}
}

func (d *Daemon) handleSignal(w http.ResponseWriter, r *http.Request, id string) {
	name := d.containerName(id)
	var req SignalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "bad request: "+err.Error())
		return
	}
	reqJSON, err := json.Marshal(AgentRequest{Cmd: "signal", ExecID: req.ExecID, Signal: req.Signal})
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	cmd := exec.Command(d.DockerBin, "exec", "-i", name, "orchicon", "runtime-client")
	cmd.Stdin = bytes.NewReader(reqJSON)
	out, err := cmd.CombinedOutput()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "signal exec: "+err.Error()+" ("+strings.TrimSpace(string(out))+")")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleExecStatus reports whether an exec is still running inside a
// runtime container (the execution-liveness reaper's query).
//
// The answer must distinguish three cases so the reaper never kills a
// healthy execution on a transient blip:
//   - container missing       -> alive:false, container:false (definitive dead)
//   - supervisor answers      -> alive:<its verdict>          (definitive)
//   - the probe itself failed -> unknown:true                 (UNDETERMINABLE)
//
// The old code swallowed the docker-exec error and defaulted to
// alive:false, so a transient docker/socket hiccup made a running exec
// look dead and the single-probe reaper reaped it.
func (d *Daemon) handleExecStatus(w http.ResponseWriter, id, execID string) {
	name := d.containerName(id)
	if running, err := d.containerRunning(name); err != nil || !running {
		writeJSON(w, http.StatusOK, map[string]bool{"alive": false, "container": false})
		return
	}
	reqJSON, err := json.Marshal(AgentRequest{Cmd: "status", ExecID: execID})
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]bool{"alive": false, "container": true})
		return
	}
	cmd := exec.Command(d.DockerBin, "exec", "-i", name, "orchicon", "runtime-client")
	cmd.Stdin = bytes.NewReader(reqJSON)
	out, perr := cmd.Output()
	if perr != nil {
		// Probe failed (docker exec hiccup, supervisor socket momentarily
		// unavailable) — not a definitive "dead". Report undeterminable so
		// the reaper skips instead of reaping a healthy execution.
		writeJSON(w, http.StatusOK, map[string]any{"alive": false, "container": true, "unknown": true})
		return
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		var ev AgentEvent
		if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.Event == "status" {
			writeJSON(w, http.StatusOK, map[string]bool{"alive": ev.Alive, "container": true})
			return
		}
	}
	// The supervisor didn't answer the status query — undeterminable.
	writeJSON(w, http.StatusOK, map[string]any{"alive": false, "container": true, "unknown": true})
}

func (d *Daemon) handleKill(w http.ResponseWriter, id string) {
	name := d.containerName(id)
	out, err := d.docker("rm", "-f", name)
	if err != nil {
		// Not running / already removed / still settling is fine.
		if strings.Contains(out, "No such container") || strings.Contains(out, "in progress") {
			writeJSON(w, http.StatusOK, map[string]string{"status": "gone"})
			return
		}
		httpError(w, http.StatusInternalServerError, "kill "+name+": "+err.Error()+" ("+strings.TrimSpace(out)+")")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
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
func validateExec(req ExecRequest) error {
	if len(req.Argv) == 0 {
		return fmt.Errorf("argv required")
	}
	base := filepath.Base(req.Argv[0])
	switch base {
	case "opencode", "orchicon", "bash", "sh":
	default:
		return fmt.Errorf("argv[0] not allowed: %s", base)
	}
	return nil
}

func (d *Daemon) docker(args ...string) (string, error) {
	cmd := exec.Command(d.DockerBin, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// signalRuntimeExec sends a signal to a running exec inside a runtime
// container via `orchicon runtime-client` (bounded; best-effort).
func (d *Daemon) signalRuntimeExec(name, execID, sig string) error {
	reqJSON, err := json.Marshal(AgentRequest{Cmd: "signal", ExecID: execID, Signal: sig})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, d.DockerBin, "exec", "-i", name, "orchicon", "runtime-client")
	cmd.Stdin = bytes.NewReader(reqJSON)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("signal %s exec %s: %v (%s)", name, execID, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

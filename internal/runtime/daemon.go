package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	UserID       int      // host uid the runtime runs as (writes project mounts)
	GroupID      int      // host gid
	HostHome     string   // host user home, mounted as the container HOME
	AllowedRoots []string // host prefixes a project mount may be under
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
}

// CreateResponse is returned by POST /v1/runtimes.
type CreateResponse struct {
	ContainerID string `json:"container_id"`
	Name        string `json:"name"`
	Running     bool   `json:"running"`
}

// ExecRequest is the body of POST /v1/runtimes/{id}/exec.
type ExecRequest struct {
	ExecID     string   `json:"exec_id"`
	Argv       []string `json:"argv"`
	Env        []string `json:"env"`
	Cwd        string   `json:"cwd"`
	ProjectDir string   `json:"project_dir"`
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
// state-aware adopt sweep is the primary (and faster) cleanup.
func (d *Daemon) sweepOrphans(ctx context.Context) {
	if d.MaxAge <= 0 {
		return
	}
	interval := d.SweepInterval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		names, err := d.listRuntimes()
		if err != nil {
			d.Log.Warn("orphan sweep: list", "error", err)
			continue
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
		names, err := d.listRuntimes()
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
	if d.Image != "" && req.Image != "" && req.Image != d.Image {
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
func (d *Daemon) createRuntime(req CreateRequest) (*CreateResponse, error) {
	name := d.containerName(req.WorkflowID)
	if running, _ := d.containerRunning(name); running {
		return &CreateResponse{Name: name, Running: true}, nil
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
		"--user", fmt.Sprintf("%d:%d", d.UserID, d.GroupID),
		"--cpus", d.CPUs,
		"--memory", d.Memory,
		"--tmpfs", "/tmp:rw,size=" + d.TmpfsSize,
	}
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
	// ephemeral FS by the supervisor's isolateOpenCodeData), and git
	// identity + credential store (read-only — PR/merge workers). These
	// are appended by the daemon, not the plane, so the control plane
	// can never request them.
	if d.HostHome != "" {
		if st, err := os.Stat(filepath.Join(d.HostHome, ".config/opencode")); err == nil && st.IsDir() {
			args = append(args, "-v", filepath.Join(d.HostHome, ".config/opencode")+":"+filepath.Join(d.HostHome, ".config/opencode")+":ro")
		}
		if st, err := os.Stat(filepath.Join(d.HostHome, ".local/share/opencode")); err == nil && st.IsDir() {
			args = append(args, "-v", filepath.Join(d.HostHome, ".local/share/opencode")+":"+filepath.Join(d.HostHome, ".local/share/opencode")+":ro")
		}
		for _, f := range []string{".gitconfig", ".git-credentials"} {
			if st, err := os.Stat(filepath.Join(d.HostHome, f)); err == nil && !st.IsDir() {
				args = append(args, "-v", filepath.Join(d.HostHome, f)+":"+filepath.Join(d.HostHome, f)+":ro")
			}
		}
		args = append(args, "-e", "HOME="+d.HostHome)
	}
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
			return &CreateResponse{Name: name, Running: true}, nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return nil, fmt.Errorf("runtime %s started but supervisor socket not ready", name)
}

func (d *Daemon) containerRunning(name string) (bool, error) {
	out, err := d.docker("inspect", "--format", "{{.State.Running}}", name)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "true", nil
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

func (d *Daemon) listRuntimes() ([]string, error) {
	out, err := d.docker("ps", "-a", "--filter", "label=orchicon.workflow", "--format", "{{.Names}}")
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
		Cmd:        "exec",
		ExecID:     req.ExecID,
		Argv:       req.Argv,
		Env:        req.Env,
		Cwd:        req.Cwd,
		ProjectDir: req.ProjectDir,
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

	// When the control plane cancels the request (wall-clock timeout,
	// workflow failure, plane shutdown), signal the in-container child
	// directly. Killing the docker exec CLI does NOT terminate the
	// daemon-side exec session, so the agent must kill the opencode child
	// via an explicit signal request.
	go func() {
		<-r.Context().Done()
		d.Log.Info("runtime exec request cancelled — signalling child", "runtime", name, "exec", req.ExecID)
		_ = d.signalRuntimeExec(name, req.ExecID, "SIGKILL")
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
// runtime container (the execution-liveness reaper's query). A missing
// container or a client error returns alive:false (200) so the caller
// treats it as dead; only a daemon outage (request failure) is an error.
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
	out, _ := cmd.Output()
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		var ev AgentEvent
		if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.Event == "status" {
			writeJSON(w, http.StatusOK, map[string]bool{"alive": ev.Alive, "container": true})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"alive": false, "container": true})
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

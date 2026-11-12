package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/beardedparrott/orchicon/internal/version"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/adapter"

	"github.com/beardedparrott/orchicon/internal/procwin"
)

// It is the ONLY process with access to the container engine (podman by default, docker supported):
// it creates/kills runtime containers and warms/leases them through the warm pool.
type Daemon struct {
	// ContainerExeDataDir is the daemon's state dir, used only to NAME a durable location in the refusal's
	// remedy (exe_resolve.go). Never the daemon's own path: on Windows that is the %TEMP% self-copy.
	ContainerExeDataDir string
	// ContainerExeRequired says this host's own executable CANNOT be the container's entrypoint, so an
	// empty ContainerExe is a refusal rather than a fallback.
	ContainerExeRequired bool
	// ContainerExe is the executable bind-mounted into runtime containers at /usr/local/bin/orchicon.
	ContainerExe string
	// Paths expresses host paths the way the ENGINE'S VM sees them (pathmap.go).
	Paths *PathMapper

	SocketPath string
	// Engine is the container engine this daemon drives (podman by default, docker supported).
	Engine       Engine
	Image        string   // image + tag, e.g. orchicon-runtime:local
	Images       []string // additional allowlisted stock image tags (base is always included)
	UserID       int      // host uid the runtime runs as (writes project mounts)
	GroupID      int      // host gid
	HostHome     string   // host user home, mounted as the container HOME
	AllowedRoots []string // host prefixes a project mount may be under
	// AllowedRootsSource, when set, supplies the roots to ADD to AllowedRoots on every check — the
	// project directories this plane knows about.
	AllowedRootsSource func(ctx context.Context) ([]string, error)
	// ExePath is the daemon's own executable, bind-mounted read-only into every runtime container at
	// /usr/local/bin/orchicon so the container can exec `orchicon runtime-supervisor` /.
	ExePath   string
	CPUs      string
	Memory    string
	TmpfsSize string
	// HomeTmpfsSize sizes the uid-owned tmpfs mounted at the runtime uid's $HOME, and at a private
	// adapter data home.
	HomeTmpfsSize string
	// MaxAge / SweepInterval are retained for config compatibility; the warm pool's idle-reap now owns
	// container cleanup (the pool is reset wholesale at daemon start.
	MaxAge        time.Duration
	SweepInterval time.Duration
	Log           *slog.Logger
	// pool leases/resets warm runtime containers (internal/runtime/pool.go).
	pool *daemonPool
	// createMu serializes createContainer so concurrent checkouts cannot race `run` on the same name
	// (the pool names are unique, but serializing the engine create path keeps the engine itself calm).
	createMu sync.Mutex
	// activeBuilds tracks in-flight engine builds by tag so Cancel can kill them.
	buildMu      sync.Mutex
	activeBuilds map[string]*exec.Cmd
	// shutdown cancels the serve loop.
	shutdownFn func()
	// Test seams: unexported func fields that override the production
	// implementations. Nil (the default) = production behavior.
	engineFn        func(args ...string) (string, error)                          // overrides engine()
	createFn        func(name string, req CreateRequest) (*CreateResponse, error) // overrides createContainer()
	ghTokenFn       func() string                                                 // overrides the ghToken resolver (default hostGHToken)
	engineVersionFn func() string                                                 // overrides the engine client-version probe (default: exec the engine)
	// engine fingerprint cache: resolved ONCE per daemon process (the pool key's engine= / shape=
	// terms).
	engineFpOnce sync.Once
	engineFp     string
	// ghToken cache: daemon-resident, TTL-bounded (ORCHICON_RUNTIME_GH_TOKEN_FP_TTL, default 60s) so
	// the pool key and the created container's GH_TOKEN come from the SAME resolution without spawning.
	ghTokenMu   sync.Mutex
	ghTokenVal  string
	ghTokenTime time.Time
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
	// InstanceID labels the runtime container with the requesting instance (dev/prod) so each control
	// plane only reaps its OWN containers.
	InstanceID string `json:"instance_id,omitempty"`
	// ServeConfig, when non-empty, requests the opencode serve inside the container.
	ServeConfig string `json:"serve_config,omitempty"`
	// HOST path; converted before anything reads it (hostpaths.go).
	ProjectDir string `json:"project_dir,omitempty" orchicon:"hostpath"`
	// Secrets are tenant secrets to inject as container env at create time.
	// Decrypted plane-side, never stored in image; same pattern as GH_TOKEN.
	Secrets map[string]string `json:"secrets,omitempty"`
	// GitStrategy is the effective workflow git strategy (local|pr|none) resolved for the run.
	GitStrategy string `json:"git_strategy,omitempty"`
	// AdapterKinds is the run's BOOT PROFILE: the set of adapter kinds its step workers resolve to
	// (adapter.AdapterKind over each step worker's model_ref), sorted and deduped by the plane.
	AdapterKinds []string `json:"adapter_kinds"`
}

// CreateResponse is returned by POST /v1/runtimes.
type CreateResponse struct {
	ContainerID string `json:"container_id"`
	Name        string `json:"name"`
	Running     bool   `json:"running"`
	// ServePort is the HOST port the plane dials for the container's opencode serve:
	// 127.0.0.1:<ServePort> under podman (the port published on loopback at create time).
	ServePort     int    `json:"serve_port,omitempty"`
	ServePassword string `json:"serve_password,omitempty"`
	ServeURL      string `json:"serve_url,omitempty"`
	// PlaneURL is the sandbox plane's /healthz base URL — http://<container ip>:8080 under docker,
	// http://127.0.0.1:<published port> under podman.
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
	mux.HandleFunc("/v1/images/inspect", d.handleImageInspect)
	mux.HandleFunc("/v1/images/build", d.handleBuildCancel)
	mux.HandleFunc("/v1/images/pull", d.handlePull)
	mux.HandleFunc("/v1/shutdown", d.handleShutdown)
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
	// /v1/shutdown drives this; srv.Close releases Serve below, which returns to the caller and
	// lets the process exit normally (the daemon holds no durable state — images live in the engine).
	stop, stopCancel := context.WithCancel(ctx)
	d.shutdownFn = stopCancel
	_ = stop
	defer func() { d.shutdownFn = nil; stopCancel() }()
	go func() {
		<-stop.Done()
		srv.Close()
	}()
	// Warm-pool lifecycle: leases are daemon-resident, so the pool starts EMPTY — but it deliberately
	// does NOT remove containers here.
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

// handleShutdown asks the daemon to exit.
func (d *Daemon) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}
	if d.shutdownFn == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "daemon is not serving"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopping"})
	go func() {
		time.Sleep(50 * time.Millisecond) // let the response flush
		d.shutdownFn()
	}()
}

func (d *Daemon) handleHealth(w http.ResponseWriter, r *http.Request) {
	// THE BUILD IS PART OF "HEALTHY", and its absence is why an eight-day-old daemon survived every
	// upgrade.
	v := version.Current()
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"tag":     v.Tag,
		"commit":  v.Commit,
		"builtAt": v.BuildDate,
	})
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
		if err := d.validateCreate(r.Context(), req); err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		// Reconcile THIS instance's pre-restart containers before leasing, so a container left by the
		// previous daemon process is neither handed out nor left leaking.
		d.pool.sweepPreRestartOrphans(instanceID(req.InstanceID))
		resp, err := d.pool.checkout(r.Context(), req.WorkflowID, req)
		if err != nil {
			// A MISSING CONTAINER BINARY IS NOT A TRANSIENT FAILURE. Tag it so the caller can stop
			// retrying instead of re-reading the same refusal once every two seconds (exe_resolve.go).
			if errors.Is(err, ErrNoContainerBinary) {
				httpErrorCode(w, http.StatusInternalServerError, err.Error(), ContainerBinaryErrCode)
				return
			}
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
//	POST   /v1/runtimes/{id}/exec  -> run one shell command in the run's
//	                                 leased container (always-container
//	                                 native transport; streams JSON-lines
//	                                 AgentEvents)
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
		// Release the run's lease: the container is removed and reset in the background back into the warm
		// pool (pristine + serve warmed).
		d.pool.release(id)
		writeJSON(w, http.StatusOK, map[string]string{"status": "released"})
	case action == "exec" && r.Method == http.MethodPost:
		d.handleRuntimeExec(w, r, id)
	case action == "stdio" && r.Method == http.MethodPost:
		d.handleRuntimeStdio(w, r, id)
	default:
		httpError(w, http.StatusNotFound, "not found")
	}
}

// ExecRequest is the body of POST /v1/runtimes/{id}/exec: one shell command to run inside the run's
// leased container (always-container native transport).
type ExecRequest struct {
	Command string   `json:"command"`
	Env     []string `json:"env,omitempty"`
	// HOST paths; converted at the one boundary (hostpaths.go).
	Cwd        string `json:"cwd,omitempty" orchicon:"hostpath"`
	ProjectDir string `json:"project_dir,omitempty" orchicon:"hostpath"`
}

// ExecResult is the terminal result of an exec: collected stdout/stderr plus the exit code (a
// non-zero exit is a worker-visible RESULT, not a transport error).
type ExecResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

// The container name resolves from the warm pool's lease table (the run MUST hold a lease — ensured
// at run start by EnsureForRun), never from a caller-supplied name.
func (d *Daemon) handleRuntimeExec(w http.ResponseWriter, r *http.Request, runID string) {
	var req ExecRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "bad request: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Command) == "" {
		httpError(w, http.StatusBadRequest, "command required")
		return
	}
	name := d.pool.containerForRun(runID)
	if name == "" {
		// LOGGED, because this handler logged nothing and that silence is what made a wedged dispatch
		// invisible.
		d.Log.Warn("runtime stdio: no runtime container leased for this id — the caller must name the RUN, not the execution",
			"run", runID)
		httpError(w, http.StatusNotFound, "no runtime container leased for run "+runID)
		return
	}
	// Cwd un-mapped is the reported failure: a Linux container cannot chdir to C:\...
	mapHostPathFields(d.Paths, &req)
	reqJSON, err := json.Marshal(AgentRequest{
		Cmd:        "exec",
		Argv:       []string{"bash", "-c", req.Command},
		Env:        req.Env,
		Cwd:        req.Cwd,
		ProjectDir: req.ProjectDir,
	})
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	cmd := procwin.Command(d.engineBin(), "exec", "-i", name, "orchicon", "runtime-client")
	cmd.Stdin = bytes.NewReader(reqJSON)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := cmd.Start(); err != nil {
		httpError(w, http.StatusInternalServerError, d.Engine.EngineNameForError()+" exec: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	// Relay the supervisor's JSON-lines AgentEvents verbatim: the plane client reassembles
	// {stream,data} chunks and the terminal {event:exit}/{event:error}.
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		_, _ = w.Write(append(sc.Bytes(), '\n'))
		if fl != nil {
			fl.Flush()
		}
	}
	_ = cmd.Wait()
}

// handleRuntimeStdio opens a long-lived DUPLEX streaming child in the run's leased container: the
// claude adapter's transport.
func (d *Daemon) handleRuntimeStdio(w http.ResponseWriter, r *http.Request, runID string) {
	// The container MUST resolve from the lease table — the plane can never
	// exec into an arbitrary container (identical gate to handleRuntimeExec).
	name := d.pool.containerForRun(runID)
	if name == "" {
		httpError(w, http.StatusNotFound, "no runtime container leased for run "+runID)
		return
	}
	rc := http.NewResponseController(w)
	if err := rc.EnableFullDuplex(); err != nil {
		d.Log.Warn("runtime stdio: EnableFullDuplex unavailable — stdin frames may not drain", "error", err)
	}
	dec := json.NewDecoder(r.Body)
	var req StdioRequest
	if err := dec.Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "bad stdio request: "+err.Error())
		return
	}
	if len(req.Argv) == 0 {
		httpError(w, http.StatusBadRequest, "stdio argv required")
		return
	}
	// Same boundary as exec: the claude adapter's cwd is a host path too.
	mapHostPathFields(d.Paths, &req)
	first, err := json.Marshal(AgentRequest{
		Cmd:        "stdio",
		Argv:       req.Argv,
		Env:        req.Env,
		Cwd:        req.Cwd,
		ProjectDir: req.ProjectDir,
		ExecID:     req.ExecID,
	})
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	cmd := procwin.Command(d.engineBin(), "exec", "-i", name, "orchicon", "runtime-client")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		httpError(w, http.StatusInternalServerError, d.Engine.EngineNameForError()+" exec: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	if fl != nil {
		fl.Flush()
	}
	if _, err := stdin.Write(append(first, '\n')); err != nil {
		_ = stdout.Close()
		_ = cmd.Wait()
		return
	}
	// Relay the child's JSON-lines stdout verbatim (flushing per line so the
	// plane sees events live).
	done := make(chan struct{})
	go func() {
		defer close(done)
		relayNDJSON(stdout, w, fl)
	}()
	// Read follow-up frames off the SAME request body and forward them.
	for {
		var f map[string]any
		if err := dec.Decode(&f); err != nil {
			break
		}
		b, merr := json.Marshal(f)
		if merr != nil {
			continue
		}
		if _, err := stdin.Write(append(b, '\n')); err != nil {
			break
		}
	}
	_ = stdin.Close()
	<-done
	_ = cmd.Wait()
}

// relayNDJSON copies newline-delimited lines from r to w, flushing after
// each line. Shared by the exec and stdio handlers.
func relayNDJSON(r io.Reader, w io.Writer, fl http.Flusher) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		if _, err := w.Write(append(sc.Bytes(), '\n')); err != nil {
			return
		}
		if fl != nil {
			fl.Flush()
		}
	}
}

// mountArgv renders the request's bind mounts as `-v <src>:<dst>[:ro]` argv.
func mountArgv(mounts []MountSpec) []string {
	out := make([]string, 0, 2*len(mounts))
	for _, m := range mounts {
		mo := ""
		if m.RO {
			mo = ":ro"
		}
		out = append(out, "-v", m.Source+":"+m.Dest+mo)
	}
	return out
}

// guestifyCreateArgv makes EVERY host path in a create argv engine-visible, and reports any that could not
// be — the last line of defence before `podman create` / `docker create` sees the vector.
//
// WHY A FINAL PASS RATHER THAN TRUSTING EACH PRODUCER. Every mount builder in this package is SUPPOSED to map
// (mountArgv via mapMounts, mapMountArgv for the adapter block, d.Paths.Map for the exe and git mounts) — but
// "supposed to" is what produced the reported failure:
//
//	create runtime orchicon-runtime-e93bfb9…: Error: invalid container path "C", must be an absolute path
//
// podman splits a volume spec on ":", so `-v C:\projects\weatherapp:...` is read as source="C",
// dest="\projects\weatherapp", and the drive letter alone reaches the container-path validator. Exit 125, no
// container, and nothing in the log saying which argument was malformed.
//
// A producer that forgets to map is easy to add and impossible to notice, because on Linux the mapping is
// identity — every test here passes and only Windows breaks. So the argv is swept ONCE at the boundary. On
// Linux this is a cheap no-op (identity mapper); on Windows it is the difference between working and not.
//
// IT REFUSES RATHER THAN GUESSING. A create argument that still holds a drive-letter path after mapping means
// something upstream is broken in a way this function cannot infer the intent of, so it returns an error naming
// the argument instead of silently shipping a mount the engine will misparse.
func guestifyCreateArgv(m *PathMapper, argv []string) ([]string, error) {
	if len(argv) == 0 {
		return argv, nil
	}
	out := make([]string, len(argv))
	copy(out, argv)
	for i, a := range out {
		if i == 0 {
			continue
		}
		switch out[i-1] {
		case "-v", "--volume":
			fixed, err := guestifyMountSpec(m, a)
			if err != nil {
				return nil, err
			}
			out[i] = fixed
		case "--tmpfs":
			// --tmpfs IS A PATH-TAKING FLAG TOO, and leaving it out is what the SECOND reported failure
			// was:
			//
			//	--tmpfs C:\Users\operator\.local\share\opencode:rw,mode=0777,size=2g
			//	→ Error: invalid container path "C", must be an absolute path
			//
			// podman parses --tmpfs as DEST[:OPTIONS], so the drive-letter colon splits it exactly as a
			// volume spec does. The first version of this sweep covered `-v` only, and every -v in that argv
			// was already /mnt/c/... — so the ONE remaining host-form argument was a tmpfs, and the guard
			// that was supposed to make a producer unable to slip looked straight past it.
			fixed, err := guestifyTmpfsSpec(m, a)
			if err != nil {
				return nil, err
			}
			out[i] = fixed
		}
	}
	return out, nil
}

// guestifyTmpfsSpec maps the DEST of a `dest[:opts]` tmpfs value, and verifies nothing host-form
// survives.
func guestifyTmpfsSpec(m *PathMapper, spec string) (string, error) {
	dest := spec
	opts := ""
	// THE SEPARATOR IS THE FIRST COLON THAT IS NOT A DRIVE LETTER.
	for j := 0; j < len(spec); j++ {
		if spec[j] != ':' {
			continue
		}
		if j == 1 && len(spec) > 2 && (spec[2] == '\\' || spec[2] == '/') {
			continue // a drive letter, not the separator
		}
		dest, opts = spec[:j], spec[j:]
		break
	}
	dest = m.Map(dest)
	if hasDriveLetter(dest) || strings.ContainsRune(dest, '\\') {
		return "", fmt.Errorf("--tmpfs argument %q still holds a Windows path after mapping (dest=%q): the "+
			"engine would split it on ':' and read the drive letter as the container path", spec, dest)
	}
	return dest + opts, nil
}

// guestifyMountSpec maps BOTH fields of one `src:dst[:opts]` that are still host paths.
func guestifyMountSpec(m *PathMapper, spec string) (string, error) {
	rest := spec
	opts := ""
	// The options suffix is the LAST ":"-separated field when it is a known flag set (ro/rw/z/Z and friends).
	if i := strings.LastIndex(rest, ":"); i >= 0 {
		if tail := rest[i+1:]; isMountOpts(tail) {
			opts = rest[i:]
			rest = rest[:i]
		}
	}
	// The destination starts at the first colon that is NOT a drive letter; see mapMountSpecString.
	split := -1
	for j := 0; j < len(rest); j++ {
		if rest[j] != ':' {
			continue
		}
		if j == 1 && len(rest) > 2 && (rest[2] == '\\' || rest[2] == '/') {
			continue // a drive letter, not the separator
		}
		split = j
		break
	}
	if split < 0 {
		return spec, nil // not a src:dst pair; leave it exactly as written
	}
	src, dst := rest[:split], rest[split+1:]
	src = m.Map(src)
	dst = m.Map(dst)
	// A DRIVE LETTER SURVIVING THE MAP IS A BUG UPSTREAM, and shipping it would hand the engine exactly the
	// argument that failed. Name it.
	if hasDriveLetter(src) || hasDriveLetter(dst) {
		return "", fmt.Errorf("create argument %q still holds a Windows path after mapping (source=%q "+
			"destination=%q): the engine would split it on ':' and read the drive letter as the container path", spec, src, dst)
	}
	return src + ":" + dst + opts, nil
}

// isMountOpts reports whether a trailing field is a mount option list rather than part of a path.
func isMountOpts(s string) bool {
	if s == "" {
		return false
	}
	for _, o := range strings.Split(s, ",") {
		switch strings.ToLower(strings.TrimSpace(o)) {
		case "ro", "rw", "z", "z,ro", "z,rw", "ro,z", "rw,z", "copy", "nocopy", "private", "shared", "slave",
			"rprivate", "rshared", "rslave", "delegated", "cached", "consistent", "U":
		default:
			return false
		}
	}
	return true
}

// hasDriveLetter reports whether p begins with a Windows drive prefix ("C:\", "C:/", "C:").
func hasDriveLetter(p string) bool {
	if len(p) < 2 {
		return false
	}
	c := p[0]
	if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')) || p[1] != ':' {
		return false
	}
	return len(p) == 2 || p[2] == '\\' || p[2] == '/'
}

// validateCreate enforces the daemon's security policy: image allowlist,
// mount-source allowlist, argv allowlist.
func (d *Daemon) validateCreate(ctx context.Context, req CreateRequest) error {
	if req.WorkflowID == "" {
		return fmt.Errorf("workflow_id required")
	}
	if !validWorkflowID(req.WorkflowID) {
		return fmt.Errorf("invalid workflow_id")
	}
	// Image allowlist: the base image, operator-configured stock images, or any image carrying the
	// inherited runtime-base label (UI-built custom images).
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
		// NO NAMED VOLUMES (, BF-3(a)): every mount source must be an ABSOLUTE HOST PATH.
		if !filepath.IsAbs(m.Source) {
			return fmt.Errorf("mount source must be an absolute host path, never a named volume (a volume survives the pristine container reset): %s", m.Source)
		}
		abs, err := filepath.Abs(m.Source)
		if err != nil {
			return fmt.Errorf("bad mount source: %w", err)
		}
		// CHECKED IN THE ENGINE'S VIEW (withinAllowedRootsIn): on Windows the host path and the guest path
		// have no string relationship.
		roots := d.effectiveAllowedRoots(ctx)
		if !withinRootsIn(d.Paths, abs, roots) {
			return fmt.Errorf("mount source outside allowed roots: %s (as the container engine sees it: %s; "+
				"allowed roots: %s — a PROJECT's own project_dir is always allowed, and "+
				"ORCHICON_RUNTIME_ALLOWED_ROOTS adds more)",
				abs, d.Paths.Map(abs), strings.Join(roots, ", "))
		}
		if m.Dest == "" {
			return fmt.Errorf("mount dest required")
		}
	}
	// Boot profile: every demanded adapter kind must be a known adapter kind (the builtin provider
	// catalog).
	known := adapter.BuiltinAdapterKinds()
	for _, k := range req.AdapterKinds {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if _, ok := known[k]; !ok {
			return fmt.Errorf("adapter kind not allowed: %s", k)
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

// effectiveAllowedRoots is the allowlist for THIS create: the operator's configured roots PLUS every project
// directory this plane knows about.
//
// WHY PROJECT DIRECTORIES ARE ALLOWED AT ALL. A runtime container exists to work on a project, and the project
// says where that is. Requiring it to live under %USERPROFILE%\projects is an arbitrary restriction with a real
// cost — an operator with their work on D:\ or C:\projects had to move the tree or discover an environment
// variable before anything would run:
//
//	mount source outside allowed roots: C:\projects\weatherapp
//	    (as the container engine sees it: /mnt/c/projects/weatherapp; allowed roots: C:\Users\operator\projects)
//
// The engine's own document that log quotes from says the default list is a CONVENIENCE, not a policy. The
// policy that matters is "this plane may only mount trees it was told to work on", and a project_dir is
// precisely that.
//
// IT IS NOT UNLIMITED ACCESS. Only directories of projects that EXIST and are ACTIVE are added; a project that
// is archived, deleted, or never created contributes nothing. So the allowlist remains a statement about what
// this plane has been asked to work on, which is the boundary that was meant.
//
// IT FAILS CLOSED. If the source cannot be read, its roots are simply absent — never "allow everything", and
// never a silent success. The operator roots still apply, so a transient database problem degrades to the old
// behaviour rather than widening anything.
func (d *Daemon) effectiveAllowedRoots(ctx context.Context) []string {
	roots := append([]string(nil), d.AllowedRoots...)
	if d.AllowedRootsSource == nil {
		return roots
	}
	extra, err := d.AllowedRootsSource(ctx)
	if err != nil {
		// LOUD BUT NOT FATAL. A container create is about to be refused for a reason the operator cannot see
		// if this stays silent; it is not fatal because the configured roots may still cover the mount.
		if d.Log != nil {
			d.Log.Warn("runtime daemon: could not read the project directories to allow — using the configured roots only",
				"error", err)
		}
		return roots
	}
	return append(roots, extra...)
}

// createContainer creates a runtime container with the given pool-managed name, warms its opencode
// serve (when ServeConfig is set), and returns its state.
func (d *Daemon) createContainer(name string, req CreateRequest) (*CreateResponse, error) {
	resp, err := d.createContainerInner(name, req)
	if err != nil {
		// THE DAEMON MUST RECORD ITS OWN FAILURES.
		d.Log.Error("runtime container create failed", "name", name, "image", req.Image,
			"error", err, "engine", d.Engine.EngineNameForError())
	}
	return resp, err
}

// createContainerInner is createContainer's body, kept separate so the failure of ANY path is logged once.
func (d *Daemon) createContainerInner(name string, req CreateRequest) (*CreateResponse, error) {
	if d.createFn != nil {
		return d.createFn(name, req)
	}

	// ProjectDir IS A CONTAINER PATH, NOT A HOST PATH. It travels into the supervisor as JSON and is used there
	// as the serve's working directory AND its safety-guard root, so a host form reaching it would make the
	// in-container serve look in a directory that cannot exist:
	//
	//	req.ProjectDir = "C:\projects\weatherapp"   →   the container's cwd does not exist
	//
	// Nothing has failed on it yet only because creation was failing earlier for other reasons; it is mapped
	// HERE, before anything reads it, so every forwarding path below is correct by the time it matters.
	mapHostPathFields(d.Paths, &req)
	// Fast, ACTIONABLE auth preflight: a claude-demanding run on a host with no reachable Claude
	// credential must fail with the exact sign-in command.
	if demandsKind(req, "claude") && !d.claudeAuthAvailable(req) {
		return nil, fmt.Errorf(adapter.ClaudeAuthRequiredMessage)
	}
	d.createMu.Lock()
	defer d.createMu.Unlock()
	// A stopped/crashed container with this name blocks recreation ("name already in use").
	if exists, _ := d.containerExists(name); exists {
		d.Log.Warn("removing stale runtime container", "name", name)
		if out, err := d.engine("rm", "-f", name); err != nil {
			return nil, fmt.Errorf("remove stale runtime %s: %s: %w", name, out, err)
		}
	}

	image := req.Image
	if image == "" {
		image = d.Image
	}
	// AN UNRUNNABLE ADAPTER IS REFUSED HERE, BEFORE THE CONTAINER EXISTS.
	if problems := CheckAdaptersForContainerWith(d.HostHome, requestedKinds(req)...); len(problems) > 0 {
		return nil, fmt.Errorf("cannot run this workflow's adapter inside a Linux runtime container: %s",
			strings.Join(problems, "; "))
	}
	args := d.createFlagArgs(name, req)
	// -v <src>:<dst>[:ro] — preserved byte-identically for both engines here.
	args = append(args, mountArgv(mapMounts(d.Paths, req.Mounts))...)
	// Standard host-home mounts.
	args = append(args, d.standardHostMountArgs(req)...)
	for k, v := range req.Secrets {
		if k == "" || v == "" {
			continue
		}
		args = append(args, "-e", k+"="+v)
	}
	// The daemon's own executable: bind-mounted read-only at /usr/local/bin/orchicon so the container
	// can exec `orchicon runtime-supervisor` (PID 1 entrypoint below) and `orchicon runtime-client`.
	containerExe := d.ContainerExe
	if containerExe == "" {
		if d.ContainerExeRequired {
			return nil, fmt.Errorf("%w: no linux/amd64 orchicon binary is available for the container's "+
				"entrypoint, and this host's own executable is a %s binary a Linux container cannot run. %s",
				ErrNoContainerBinary, binaryKind(GOOS()), containerExeRemedy(d.ContainerExeDataDir))
		}
		containerExe = d.ExePath
	}
	if containerExe == "" {
		return nil, fmt.Errorf("runtime container executable path unavailable (os.Executable failed) — cannot create runtime container: the orchicon binary is bind-mounted into every runtime container, never baked")
	}
	if st, err := os.Stat(containerExe); err != nil || st.IsDir() {
		return nil, fmt.Errorf("runtime container executable %s missing — cannot create runtime container: the orchicon binary is bind-mounted into every runtime container, never baked", containerExe)
	}
	// AND WHEN THE DAEMON KNOWS IT NEEDS A LINUX BINARY, IT DOES NOT TRUST THE RESOLUTION BLINDLY: a PE
	// at this path is the reported failure arriving by another route (a stale copy.
	if d.ContainerExeRequired && !isELFFile(containerExe) {
		return nil, fmt.Errorf("%w: %s is not a linux/amd64 binary (a Linux container cannot execute it, so "+
			"its supervisor would die on exec format error). %s",
			ErrNoContainerBinary, containerExe, containerExeRemedy(d.ContainerExeDataDir))
	}
	// SOURCE mapped (the VM must resolve it), DEST already a guest path.
	args = append(args, "-v", d.Paths.Map(containerExe)+":/usr/local/bin/orchicon:ro")
	args = append(args, image, "orchicon", "runtime-supervisor")

	// THE RESOLVED ADAPTER SURFACE, SAID OUT LOUD AT CREATE TIME.
	if d.Log != nil {
		d.Log.Info("runtime container: resolved adapter surface",
			"container", name,
			"image", image,
			"boot_profile", strings.Join(requestedKinds(req), ","),
			"adapter_mounts", strings.Join(describeAdapterMounts(d.HostHome, requestedKinds(req)...), ","))
	}
	// Create the container, retrying past name conflicts.
	args, gerr := guestifyCreateArgv(d.Paths, args)
	if gerr != nil {
		// THE ARGV IS IN THE MESSAGE.
		return nil, fmt.Errorf("create runtime %s: %w\n  engine argv: %s", name, gerr, describeCreateArgv(args))
	}
	var out string
	var err error
	created := false
	tokens := d.Engine.NameInUseTokens()
	for attempt := 0; attempt < 3 && !created; attempt++ {
		out, err = d.engine(args...)
		if err == nil {
			created = true
			break
		}
		if !nameInUse(out, tokens) {
			return nil, fmt.Errorf("create runtime %s: %s: %w\n  engine argv: %s",
				name, out, err, describeCreateArgv(args))
		}
		d.Log.Warn("runtime create conflict — removing stale container and retrying",
			"name", name, "attempt", attempt+1, "engine", d.Engine.EngineNameForError())
		_, _ = d.engine("rm", "-f", name)
		time.Sleep(500 * time.Millisecond)
	}
	if !created {
		return nil, fmt.Errorf("create runtime %s: %s: %w\n  engine argv: %s",
			name, out, err, describeCreateArgv(args))
	}

	// Wait for the in-container supervisor socket to answer pings.
	deadline := time.Now().Add(supervisorReadyWindow)
	for time.Now().Before(deadline) {
		if d.pingRuntime(name) {
			resp := &CreateResponse{Name: name, Running: true}
			// Warm the IN-CONTAINER serves the run's boot profile needs (AC 2/AC 5).
			if req.ServeConfig != "" {
				for _, kind := range serveKindsFor(requestedKinds(req)) {
					// Start the adapter's serve inside the container and publish its port.
					port, pw, planeEnabled, serr := d.startServe(name, kind, req)
					if serr != nil {
						return nil, fmt.Errorf("start %s serve in runtime %s: %w", kind, name, serr)
					}
					// Resolve where the plane DIALS this serve.
					serveBase, hostPort, berr := d.hostBaseURL(name, port)
					if berr != nil {
						return nil, fmt.Errorf("resolve host address for %s serve in runtime %s: %w", kind, name, berr)
					}
					resp.ServePort = hostPort
					resp.ServePassword = pw
					resp.ServeURL = serveBase
					if planeEnabled {
						// The supervisor booted the sandbox plane (dev image); the run-start gate verifies its /healthz
						// before any execution dispatches.
						planeBase, _, perr := d.hostBaseURL(name, sandboxPlanePort)
						if perr != nil {
							return nil, fmt.Errorf("resolve host address for the sandbox plane in runtime %s: %w", name, perr)
						}
						resp.PlaneURL = planeBase
					}
				}
			}
			return resp, nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	// A TIMEOUT WITH NO CAUSE IS THE REPORTED COMPLAINT.
	return nil, fmt.Errorf("runtime %s started but its supervisor never answered within %s%s",
		name, supervisorReadyWindow, d.supervisorFailureDetail(name))
}

// supervisorFailureDetail is the WHY behind a supervisor readiness timeout, appended to the error.
func (d *Daemon) supervisorFailureDetail(name string) string {
	var b strings.Builder
	if out, err := d.engine("inspect", "--format", "{{.State.Status}} {{.State.ExitCode}}", name); err == nil {
		if f := strings.Fields(strings.TrimSpace(out)); len(f) > 0 {
			b.WriteString("; container state: " + f[0])
			if len(f) > 1 && f[1] != "0" {
				b.WriteString(" (exit " + f[1] + ")")
			}
		}
	}
	// The failure that motivated this — exec format error on a PE entrypoint — appears ONLY here,
	// because the process died before it could log anything of its own.
	if out, err := d.engine("logs", "--tail", "5", name); err == nil {
		if line := firstNonEmptyLine(out); line != "" {
			b.WriteString("; last output: " + line)
		}
	}
	if b.Len() == 0 {
		return "" // nothing readable: say nothing rather than invent a cause
	}
	return b.String()
}

// supervisorReadyWindow is the readiness budget, named so the error can state it.
const supervisorReadyWindow = 30 * time.Second

// firstNonEmptyLine returns the first non-blank line of s, trimmed, or "".
func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// createFlagArgs is the engine-flag prefix of a runtime create: identity,
// ownership labels, resources, the private /tmp, and the published ports.
// Factored out of createContainer so the exact flag set this daemon emits is
// assertable without an engine binary (daemon_createflags_test.go pins it).
//
// EVERY element below was verified against both engines' CLIs. Where the two
// engines differ, the difference is made by Engine (engine.go) rather than by a
// literal here, so the branches sit next to each other and cannot drift:
//
//	run -d --name <n>                            both: identical CLI
//	--label orchicon.workflow=/instance=         both: our ownership marker
//	--add-host host.docker.internal:host-gateway both: podman supports
//	                                             host-gateway as well; the
//	                                             host.docker.internal SPELLING
//	                                             is kept so planePublicURL's
//	                                             string stays platform-
//
// independent
//
//	--user <uid>:<gid>                           both: PRESERVED — every file
//	                                             the container writes into a
//	                                             bind-mounted project is the
//	                                             operator's, never root (AC 6)
//	--userns=keep-id                             podman only: without it a
//	                                             rootless --user resolves into
//	                                             the SUBUID range and the
//	                                             project dir is written as an
//	                                             unmapped uid
//	--cpus <n>                                   both: core count
//	--memory <n>                                 both: identical (4g accepted)
//	--tmpfs /tmp:rw,size=<n>,exec                both: tmpfs defaults to
//	                                             noexec, and Go's TMPDIR plus
//	                                             the /tmp/orchicon-guard-* shim
//	                                             need exec
//	-p 127.0.0.1::4096 -p 127.0.0.1::8080        podman only: a rootless
//	                                             container's IP is not host-
//	                                             routable, so the loopback
//	                                             publish IS the address the
//
// plane dials;
//
//	docker keeps dialing the
//	bridge IP
//
// The published ports are the two FIXED container ports (defaultServePort,
// sandboxPlanePort), which is what makes publishing at create time possible.
// Publishing at create rather than on first accept is deliberate: the port
// must exist before the serve can be reached, and the daemon re-resolves it
// after startServe and validates it with serveUsableAt before the plane sees it
// ( — ordering answers the proxy race, not luck).
func (d *Daemon) createFlagArgs(name string, req CreateRequest) []string {
	workflowLabel := req.WorkflowID
	if workflowLabel == "" {
		workflowLabel = "pool"
	}
	args := d.Engine.CreateBaseArgs(name, workflowLabel, instanceID(req.InstanceID))
	args = append(args, "--user", fmt.Sprintf("%d:%d", d.UserID, d.GroupID))
	args = append(args, d.Engine.UsernsArgs()...)
	args = append(args,
		"--cpus", d.CPUs,
		"--memory", d.Memory,
		// A private tmpfs sized to TmpfsSize, mountable exec.
		"--tmpfs", fmt.Sprintf("/tmp:rw,size=%s,exec", d.TmpfsSize),
	)
	args = append(args, d.Engine.PublishArgs(defaultServePort, sandboxPlanePort)...)
	return args
}

// standardHostMountArgs returns the daemon-appended host-home arg block for a create request: the
// ADAPTER install mounts (conditional on the run's boot profile — AC 1/AC 3).
func (d *Daemon) standardHostMountArgs(req CreateRequest) []string {
	if d.HostHome == "" {
		return nil
	}
	var args []string
	// Adapter installs — mounted ONLY when the run's boot profile demands the kind.
	args = append(args, mapMountArgv(d.Paths, adapterHostMounts(d.HostHome, requestedKinds(req)...))...)
	// PRIVATE adapter directories: a demand whose install is declared private gets a writable,
	// runtime-uid-owned directory at the same path instead of a host bind.
	for _, ph := range adapterPrivateHomes(d.HostHome, requestedKinds(req)...) {
		// A PRIVATE FILE IS CONFIG, NOT A HOME.
		if ph.isFile {
			args = append(args, "-v", d.Paths.Map(ph.dir)+":"+d.Paths.Map(ph.dir)+":rw")
			continue
		}
		// MAPPED, LIKE EVERY OTHER PATH THIS FUNCTION EMITS.
		args = append(args, "--tmpfs", d.tmpfsOwned(d.Paths.Map(ph.dir), d.HomeTmpfsSize))
		if ph.authFile != "" {
			// WRITABLE ONLY WHEN THE ADAPTER DECLARES IT NEEDS TO BE (adapterInstall.authWritable).
			mode := ":ro"
			if ph.authWritable {
				mode = ":rw"
			}
			args = append(args, "-v", d.Paths.Map(ph.authFile)+":"+
				d.Paths.Map(filepath.Join(ph.dir, filepath.Base(ph.authFile)))+mode)
		}
	}
	// Git identity/credential + push-capable GH_TOKEN are gated on the run's effective git strategy: a
	// "none" (ephemeral detached-HEAD) run must never be able to create or push a branch.
	if req.GitStrategy != "none" {
		for _, f := range []string{".gitconfig", ".git-credentials"} {
			if st, err := os.Stat(filepath.Join(d.HostHome, f)); err == nil && !st.IsDir() {
				hp := filepath.Join(d.HostHome, f)
				args = append(args, "-v", d.Paths.Map(hp)+":"+d.Paths.Map(hp)+":ro")
			}
		}
		// GitHub CLI auth + state (read-only — PR/merge workers run `gh pr create`/`gh repo create`).
		if st, err := os.Stat(filepath.Join(d.HostHome, ".config", "gh")); err == nil && st.IsDir() {
			hp := filepath.Join(d.HostHome, ".config", "gh")
			args = append(args, "-v", d.Paths.Map(hp)+":"+d.Paths.Map(hp)+":ro")
		}
		if st, err := os.Stat(filepath.Join(d.HostHome, ".local", "share", "gh")); err == nil && st.IsDir() {
			hp := filepath.Join(d.HostHome, ".local", "share", "gh")
			args = append(args, "-v", d.Paths.Map(hp)+":"+d.Paths.Map(hp)+":ro")
		}
		// The operator's gh token often lives in the OS keyring, which is NOT available inside containers —
		// hosts.yml alone has no token, so `gh` reports "not authenticated".
		if tok := d.ghToken(); tok != "" {
			args = append(args, "-e", "GH_TOKEN="+tok)
		}
	}
	// $HOME ITSELF MUST BE OWNED BY THE RUNTIME UID. Docker creates a bind
	// destination's missing parents — $HOME included — as root:0755, so the
	// runtime uid could not create ANYTHING under its own home: the adapter CLI
	// exits 1 at boot on `EACCES mkdir $HOME/.local/state`, and ensureHomeDirs
	// (cmd/orchicon/runtime.go), which creates all six of the standard
	// directories best-effort, was failing on every one of them behind a WARN
	// nobody read. `.local` and `.config` need their OWN entry because a bind's
	// intermediate parent is created root-owned even INSIDE a uid-owned $HOME —
	// mounting $HOME alone was measured NOT to fix it.
	// A TMPFS ON THE PARENT OF A BIND-MOUNTED CHILD WAS REPORTED AS A SHADOWING HAZARD. IT IS NOT ONE —
	// MEASURED, not reasoned about, with the engine this actually runs on:
	//
	//	$ podman run -v /h/.config/opencode:/h/.config/opencode:ro --tmpfs /h/.config:rw,mode=0777 …
	//	$ mount | grep /h/.config
	//	  tmpfs on /h/.config            type tmpfs (rw,mode=777)
	//	  tmpfs on /h/.config/opencode   type tmpfs (ro)      <- the bind, still there
	//	$ cat /h/.config/opencode/opencode.json
	//	  {"cfg":"present"}                                   <- readable
	//
	// and the REVERSED order gives the same result, so it is not "podman happens to apply the deeper path
	// last by luck" — a bind over a tmpfs-covered directory mounts independently of the tmpfs on its parent,
	// in either order.
	//
	// RECORDED BECAUSE THE HAZARD IS PLAUSIBLE and will be suggested again. The entries are also load-bearing
	// — they exist so the RUNTIME UID can create its own state under a home whose parents the engine creates
	// root-owned — so removing them to "avoid" a shadow would break the writable home for no gain.
	//
	// THE HOME PATHS ARE GUEST PATHS, AND THEY MUST BE MAPPED. `--tmpfs` and `HOME=` are consumed by the
	// ENGINE, which lives in a Linux VM on Windows and macOS — so they need the /mnt/c/... form exactly as
	// every bind does. This block used the RAW HOST PATH and was never exercised on Windows because the
	// adapter refusal killed the create first (see CheckAdaptersForContainer); once that is fixed it would
	// have tried `--tmpfs C:\Users\operator:...`, which the engine rejects, and `HOME=C:\Users\operator`,
	// which no Linux process can mkdir into.
	homeDirs := []struct {
		path string
		size string
	}{
		{d.Paths.Map(d.HostHome), d.HomeTmpfsSize},
		{d.Paths.Map(filepath.Join(d.HostHome, ".local")), ""},
		{d.Paths.Map(filepath.Join(d.HostHome, ".config")), ""},
	}
	for _, hd := range homeDirs {
		args = append(args, "--tmpfs", d.tmpfsOwned(hd.path, hd.size))
	}
	args = append(args, "-e", "HOME="+d.Paths.Map(d.HostHome))
	// Per-kind (not just the default kind), and ONLY for a demanded kind whose host bin dir actually
	// exists — the prefix never points at an absent host dir (the invariant above).
	kinds := requestedKinds(req)
	if dirs := adapterPathPrefix(d.HostHome, kinds...); len(dirs) > 0 {
		args = append(args, "-e", "PATH="+strings.Join(dirs, string(os.PathListSeparator))+":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
		// Tell the supervisor (agentEnv) which kinds were actually mounted,
		// so a CHILD process gets the same bin dirs prepended.
		args = append(args, "-e", "ORCHICON_ADAPTER_KINDS="+strings.Join(kinds, ","))
	}
	// TELL THE SUPERVISOR WHICH HOST PATHS ARE MOUNTED WHERE, so it can repoint the worktree `.git`
	// pointers the plane provisioned on this side (gitdir.go).
	if mapping := gitdirMappingEnv(req.Mounts, d.Paths); mapping != "" {
		args = append(args, "-e", mapping)
	}
	// Provider credentials for a demanded adapter, as ENVIRONMENT.
	if pairs, unusable := adapterCredentialEnv(d.HostHome, kinds...); len(pairs) > 0 {
		for _, p := range pairs {
			args = append(args, "-e", p)
		}
		if len(unusable) > 0 && d.Log != nil {
			d.Log.Info("adapter credentials: providers are signed in on the host but cannot be "+
				"delivered through the environment (an oauth entry, or an id with no known env "+
				"variable) — a container session cannot use them", "providers", unusable)
		}
	}
	return args
}

// tmpfsOwned renders the --tmpfs option string for a private mount at path that
// the RUNTIME UID can write, which is the entire point of emitting one: a tmpfs
// left at the engine's default is root-owned, the runtime uid cannot write it,
// and the supervisor's own home setup then fails behind a WARN nobody reads.
//
// THE OWNERSHIP MECHANISM IS PER ENGINE FAMILY, because the two engines do not
// accept the same grammar. Measured, not assumed (podman 6.1.0, docker 29.7.2):
//
//	docker: --tmpfs /p:rw,uid=1000,gid=1000,mode=0755   -> accepted
//	podman: --tmpfs /p:rw,uid=1000,gid=1000,mode=0755   -> "Error: unknown mount
//	        option \"uid=1000\": invalid mount option", exit 125
//	podman: --tmpfs /p:rw,mode=0755                     -> accepted
//	podman: --mount type=tmpfs,...,uid=1000             -> "uid: invalid mount option"
//
// So podman takes `mode=` and REJECTS `uid=`/`gid=`, and `mode=` is the portable
// intersection both engines honour. Docker keeps the precise ownership it always
// had; podman gets the write access through the MODE instead.
//
// MODE=0777, NOT 0755 AND NOT 1777, and both halves were measured as uid 1000
// under rootless `--userns=keep-id`:
//
//   - 0755 -> the supervisor cannot create $HOME/.local or $HOME/.config inside a
//     root-owned directory: "touch: cannot touch '/tmp/mnt/probe': Permission
//     denied".
//   - 1777 (sticky) does permit the creation, but the sticky bit forbids DELETING
//     any entry the caller does not own, and every entry here is created by the
//     runtime uid inside a ROOT-owned directory — so a stale entry could never be
//     replaced.
//   - 0777 permits both. These mounts are private to one container for one run and
//     every process inside runs as the same runtime uid, so there is no second
//     user for a restrictive mode to protect against; the permissive mode IS the
//     mechanism that replaces the ownership options podman will not take.
//
// THIS IS ONE CODE PATH ON LINUX, macOS AND WINDOWS. The ENGINE FAMILY decides
// the grammar, never GOOS: on macOS and Windows podman runs inside its `podman
// machine` VM and parses these options with the same podman code as Linux, so the
// podman argv proved here is the correct one there too. Keying on GOOS would have
// handed those hosts a grammar this bug already proved podman refuses.
//
// size == "" omits the limit, which is right for the mount points that never hold
// file data.
// tmpfsOwned renders a `--tmpfs` value whose path the RUNTIME UID can write.
//
// THE PATH IS MAPPED HERE, IN THE ONE BUILDER, so every caller is correct by construction. It was not, and that
// is exactly how the reported failure happened: the opencode private data home was the ONE argument left in
// host form in a vector where every -v was already /mnt/c/…, so podman read `--tmpfs C:\…:rw` as the container
// path "C" and refused the create. A per-call-site mapping is a rule someone can forget; a mapping inside the
// only builder of that flag is a rule nobody has to remember.
func (d *Daemon) tmpfsOwned(path, size string) string {
	path = d.Paths.Map(path)
	opt := path + ":rw"
	if d.Engine.isPodman() {
		opt += ",mode=0777"
	} else {
		opt += fmt.Sprintf(",uid=%d,gid=%d,mode=0755", d.UserID, d.GroupID)
	}
	if size != "" {
		opt += ",size=" + size
	}
	return opt
}

// claudeAuthAvailable reports whether the host has a usable Claude credential, so a
// claude-demanding create can fail ACTIONABLY instead of hanging.
func (d *Daemon) claudeAuthAvailable(req CreateRequest) bool {
	if d.HostHome != "" {
		if st, err := os.Stat(filepath.Join(d.HostHome, ".claude", ".credentials.json")); err == nil && !st.IsDir() {
			return true
		}
	}
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		return true
	}
	_, ok := req.Secrets["ANTHROPIC_API_KEY"]
	return ok
}

// hostGHToken resolves the operator's effective GitHub CLI token (`gh auth token`), which may live
// in the OS keyring rather than hosts.yml.
func hostGHToken() string {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := procwin.CommandContext(ctx, "gh", "auth", "token")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ghToken returns the host's effective GH token, cached for ORCHICON_RUNTIME_GH_TOKEN_FP_TTL
// (default 60s).
func (d *Daemon) ghToken() string {
	d.ghTokenMu.Lock()
	defer d.ghTokenMu.Unlock()
	ttl := 60 * time.Second
	if v := d.envDuration("ORCHICON_RUNTIME_GH_TOKEN_FP_TTL"); v > 0 {
		ttl = v
	}
	if d.ghTokenTime.IsZero() || time.Since(d.ghTokenTime) > ttl {
		resolve := d.ghTokenFn
		if resolve == nil {
			resolve = hostGHToken
		}
		d.ghTokenVal = resolve()
		d.ghTokenTime = time.Now()
	}
	return d.ghTokenVal
}

// The version NEVER degrades to "": an empty term would fold away and silently stop covering the
// engine — the exact failure a fingerprint exists to prevent.
func (d *Daemon) engineInputFingerprint() string {
	d.engineFpOnce.Do(func() {
		ver := ""
		if d.engineVersionFn != nil {
			ver = strings.TrimSpace(d.engineVersionFn())
		} else if _, lookErr := exec.LookPath(d.Engine.binary()); lookErr == nil {
			if out, err := d.engine(d.Engine.VersionArgs()...); err == nil {
				ver = strings.TrimSpace(out)
			}
		}
		if ver == "" {
			ver = "unknown"
		}
		d.engineFp = "engine=" + d.Engine.name() + "@" + ver + "|shape=" + d.Engine.Shape()
	})
	return d.engineFp
}

// hostInputsFingerprint computes a fresh-per-checkout fingerprint of the read-once HOST inputs
// baked into every runtime container at create time.
func (d *Daemon) hostInputsFingerprint() string {
	// Binary fingerprint FIRST — the bind-mount applies to EVERY runtime container (createContainer
	// hard-fails without it), so it must color the pool key even when HostHome is unset.
	var binFp string
	if st, err := os.Stat(d.ExePath); err == nil && !st.IsDir() {
		// Content hash of the daemon's own executable (hashFileContent returns a distinct sentinel when the
		// file exists but cannot be read — never an empty "absent" value).
		binFp = fmt.Sprintf("size=%d|%s", st.Size(), hashFileContent(d.ExePath))
	} else {
		binFp = "absent"
	}
	hostFp := ""
	if d.HostHome != "" {
		hostFp = hostInputsFingerprint(d.HostHome, ghTokenFingerprint(d.ghToken()))
	}
	return hostFp + "|" + binFp + "|" + d.engineInputFingerprint()
}

// startServe asks the in-container supervisor to bring up `opencode serve` (idempotent.
func (d *Daemon) startServe(name, kind string, req CreateRequest) (int, string, bool, error) {
	reqJSON, err := json.Marshal(AgentRequest{
		Cmd: "serve",
		// The demanded adapter kind whose serve this starts.
		AdapterKind: kind,
		Argv:        []string{"opencode", "serve", "--hostname", "0.0.0.0", "--port", "4096"},
		Env: []string{
			"OPENCODE_CONFIG_CONTENT=" + req.ServeConfig,
		},
		Cwd:        req.ProjectDir,
		ProjectDir: req.ProjectDir,
	})
	if err != nil {
		return 0, "", false, err
	}
	cmd := procwin.Command(d.engineBin(), "exec", "-i", name, "orchicon", "runtime-client")
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

	// Resolve where the plane reaches this serve: podman's published loopback port, or docker's
	// container IP.
	serveBase, _, berr := d.hostBaseURL(name, port)
	if berr != nil {
		return 0, "", false, fmt.Errorf("resolve serve address for %s: %w", name, berr)
	}

	// The serve's cold start answers /global/health before it can handle sessions (providers/MCP load);
	// gate on the serve being USABLE — a real session create round-trip.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if serveUsableAt(serveBase, password) {
			// Give the accept path a beat after the first success.
			time.Sleep(500 * time.Millisecond)
			return port, password, planeEnabled, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return 0, "", false, fmt.Errorf("serve %s did not become usable within 30s", serveBase)
}

// serveUsableAt is the L1 serve-readiness probe (the workflow run-start gate): the serve must
// answer /global/health AND accept a real session-create round-trip.
func serveUsableAt(baseURL, password string) bool {
	client := &http.Client{Timeout: 3 * time.Second}

	// EACH STEP TRIES EVERY PATH A SERVE MAY EXPOSE (see opencodeapi.go).
	if !anyPathOK(client, baseURL, password, http.MethodGet, OpenCodeHealthPaths(), nil, func(code int) bool {
		return code == http.StatusOK
	}) {
		return false
	}

	// The serve is healthy; confirm it can actually create a session (the
	// cold-start window answers health before the session machinery is up).
	body := `{"title":"orchicon-serve-probe"}`
	return anyPathOK(client, baseURL, password, http.MethodPost,
		OpenCodeSessionCreatePaths(), []byte(body), opencodeAcceptableCreateStatus)
}

// anyPathOK issues the request against each candidate path until one answers acceptably, and
// reports whether any did.
func anyPathOK(client *http.Client, baseURL, password, method string, paths []string, body []byte, accept func(int) bool) bool {
	for _, path := range paths {
		var rdr io.Reader
		if body != nil {
			rdr = bytes.NewReader(body)
		}
		req, err := http.NewRequest(method, baseURL+path, rdr)
		if err != nil {
			continue
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.SetBasicAuth("opencode", password)
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		ok := accept(resp.StatusCode)
		// Drain before closing so the connection can be reused, and so the probe session's body does
		// not hold the socket open.
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if ok {
			return true
		}
	}
	return false
}

// hostBaseURL resolves the host-reachable base URL for one of a container's ports, plus the host
// port to ADVERTISE.
func (d *Daemon) hostBaseURL(name string, cport int) (string, int, error) {
	host, port, err := d.Engine.HostEndpoint(name, cport, d.engine)
	if err != nil {
		return "", 0, err
	}
	return fmt.Sprintf("http://%s:%d", host, port), port, nil
}

// engineBin is the executable to run: the resolved engine binary, else the
// default engine's name (so a zero-valued Engine never execs "").
func (d *Daemon) engineBin() string { return d.Engine.binary() }

func (d *Daemon) containerRunning(name string) (bool, error) {
	out, err := d.engine("inspect", "--format", "{{.State.Running}}", name)
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
	out, err := d.engine("inspect", "--format", "{{.Id}}", name)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// containerGoneWording reports whether an engine error means the container is ALREADY GONE —
// success for a removal's purposes.
func containerGoneWording(out string) bool {
	lower := strings.ToLower(out)
	for _, t := range []string{"no such container", "no container with name", "does not exist", "no such object"} {
		if strings.Contains(lower, t) {
			return true
		}
	}
	return false
}

// waitContainerGone polls until the engine reports the container absent or the timeout expires.
func (d *Daemon) waitContainerGone(name string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		exists, _ := d.containerExists(name)
		if !exists {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// removeContainer discards a runtime container and WAITS until the engine has released the NAME,
// because the pristine reset recreates from a fresh name and a lingering name turns a reset.
func (d *Daemon) removeContainer(name string) bool {
	if out, err := d.engine("rm", "-f", name); err != nil && !containerGoneWording(out) {
		d.Log.Warn("remove runtime container", "container", name, "engine", strings.TrimSpace(out), "error", err)
	}
	if d.waitContainerGone(name, 5*time.Second) {
		return true
	}
	// The name is somehow still there: one more removal, then a short re-poll.
	_, _ = d.engine("rm", "-f", name)
	gone := d.waitContainerGone(name, 2*time.Second)
	if !gone {
		d.Log.Warn("runtime container still present after removal — dropping it from the pool", "container", name)
	}
	return gone
}

// listRuntimes returns runtime container names, optionally scoped to one instance (empty instance =
// all instances).
func (d *Daemon) listRuntimes(instance string) ([]string, error) {
	args := []string{"ps", "-a", "--filter", "label=orchicon.workflow"}
	if instance != "" {
		args = append(args, "--filter", "label=orchicon.instance="+instance)
	}
	args = append(args, "--format", "{{.Names}}")
	out, err := d.engine(args...)
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

// pingRuntime runs a bounded readiness ping against the container's supervisor socket via `orchicon
// runtime-client`.
func (d *Daemon) pingRuntime(name string) bool {
	req, _ := json.Marshal(AgentRequest{Cmd: "ping"})
	cmd := procwin.Command(d.engineBin(), "exec", "-i", name, "orchicon", "runtime-client")
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

// engine runs ONE container-engine CLI invocation and returns its combined stdout+stderr.
func (d *Daemon) engine(args ...string) (string, error) {
	if d.engineFn != nil {
		return d.engineFn(args...)
	}
	cmd := procwin.Command(d.engineBin(), args...)
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

// httpErrorCode is httpError plus a machine-readable reason, for the failures a retry cannot fix. The body keeps
// its "error" key so every existing reader still works; "code" is additive.
func httpErrorCode(w http.ResponseWriter, code int, msg, errCode string) {
	writeJSON(w, code, map[string]string{"error": msg, "code": errCode})
}

// describeAdapterMounts lists the HOST paths this create would mount for the demanded adapter
// kinds, so a missing one is visible in the log rather than inferred from its absence in a string.
func describeAdapterMounts(home string, kinds ...string) []string {
	if home == "" {
		return nil
	}
	var out []string
	for _, a := range adapterHostMounts(home, kinds...) {
		// adapterHostMounts returns a flag/value list: keep the values that are mounts.
		if strings.HasPrefix(a, "/") || (len(a) > 1 && a[1] == ':') {
			out = append(out, a)
		}
	}
	for _, ph := range adapterPrivateHomes(home, kinds...) {
		if ph.isFile {
			out = append(out, ph.dir)
			continue
		}
		out = append(out, ph.dir)
		if ph.authFile != "" {
			out = append(out, ph.authFile)
		}
	}
	sort.Strings(out)
	return out
}

// describeCreateArgv renders a create argv for an error message: one argument per line, REDACTED.//
// describeCreateArgv renders a create argv for an error message: one argument per line, REDACTED.
func describeCreateArgv(argv []string) string {
	var b strings.Builder
	for i, a := range argv {
		if i > 0 {
			b.WriteString("\n  ")
		}
		if i > 0 && (argv[i-1] == "-e" || argv[i-1] == "--env") {
			b.WriteString(redactEnvArg(a))
			continue
		}
		b.WriteString(a)
	}
	return b.String()
}

// redactEnvArg keeps an `-e KEY=VALUE` argument's KEY and withholds the value.
func redactEnvArg(a string) string {
	if i := strings.IndexByte(a, '='); i > 0 {
		return a[:i+1] + "<redacted>"
	}
	return a
}

// gitdirMappingEnv renders the host|guest prefix pairs for the mounts whose two sides DIFFER. ONLY
// DIFFERING MOUNTS ARE EMITTED.
func gitdirMappingEnv(mounts []MountSpec, m *PathMapper) string {
	if m == nil || m.Mode() == PathModeIdentity {
		return ""
	}
	var parts []string
	seen := make(map[string]bool, len(mounts))
	for _, ms := range mounts {
		if ms.Source == "" || ms.Dest == "" {
			continue
		}
		// THE HOST SIDE IS THE RAW SOURCE.
		host := strings.TrimRight(ms.Source, "/")
		if back, ok := m.Unmap(host); ok {
			host = strings.TrimRight(back, "/")
		}
		guest := strings.TrimRight(m.Map(ms.Dest), "/")
		if host == "" || guest == "" || host == guest {
			continue
		}
		if seen[guest] {
			continue
		}
		seen[guest] = true
		parts = append(parts, host+"|"+guest)
	}
	if len(parts) == 0 {
		return ""
	}
	return envGitdirMaps + "=" + strings.Join(parts, ";")
}

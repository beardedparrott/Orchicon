package instancectl

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/instance"
	"github.com/beardedparrott/orchicon/internal/planebind"
	"github.com/beardedparrott/orchicon/internal/planecontrol"
)

// Instance is one manageable server: what it is called, where its state lives, what it is doing.
//
// It is the DTO both GUIs render. Deliberately flat and JSON-friendly — the desktop receives it through
// Wails and the TUI through a plain call, and neither should have to reshape it.
type Instance struct {
	Name    string `json:"name"`
	DataDir string `json:"dataDir"`
	// ControlPort is the port its plane serves on, 0 when unknown.
	ControlPort int `json:"controlPort"`
	// URL is where the web UI would be, "" when the port is unknown.
	URL string `json:"url"`
	// Plane is the plane's state DTO — the same one the desktop already renders.
	Plane planebind.StatusDTO `json:"plane"`
	// Setup is the in-flight (or last) setup, when a progress record exists.
	Setup *SetupProgress `json:"setup,omitempty"`
	// Known is whether this name is one of the built-in instances (dev/prod/test). A hand-created
	// instance is still manageable — it is merely not one of the three the port table pre-books.
	Known bool `json:"known"`
}

// Options carries what a caller may decide about an operation. Zero values take the instance's own
// recorded defaults, so both GUIs can pass nothing and get the same answer as the CLI would.
type Options struct {
	// Purge destroys the instance's data on uninstall. IRREVERSIBLE, and the only path that removes the
	// database cluster or the secrets key. The GUI must confirm before setting it.
	Purge bool
	// RuntimeImages selects the optional runtime images at install: "none", "all", or "1,3".
	// "" leaves the CLI's own default.
	RuntimeImages string
	// Ports pins specific ports (ORCHICON_*_PORT names). Empty lets the instance's defaults apply —
	// which is what a reinstall MUST do, or it renumbers an existing instance's database.
	Ports map[string]int
	// Out receives the operation's progress text. nil discards it (a GUI reads SetupProgress instead).
	Out interface{ Write([]byte) (int, error) }
}

// Result is what an operation did, in the shape the desktop's message layer already renders
// (frontend/src/lib/manage-server.ts describeAction).
type Result struct {
	Action   string `json:"action"`   // install | uninstall | start | stop | restart
	Instance string `json:"instance"` // "" for an all-instances uninstall
	// Outcome is a stable, machine-readable word. The UI maps it to a sentence; the backend must never
	// return prose here, or the two GUIs drift in their wording.
	Outcome string `json:"outcome"`
	OK      bool   `json:"ok"`
	// Message is the human explanation, for a log or a tooltip.
	Message string `json:"message"`
	// LogPath is where the evidence is when Outcome is "failed".
	LogPath string `json:"logPath"`
}

// Outcomes. A closed set, because a GUI switch over free-form strings is how one client silently stops
// rendering a case the other handles.
const (
	OutcomeInstalled         = "installed"
	OutcomeUninstalled       = "uninstalled"
	OutcomePurged            = "purged"
	OutcomeStarted           = "started"
	OutcomeAlreadyRunning    = "already-running"
	OutcomeStopped           = "stopped"
	OutcomeAlreadyStopped    = "already-stopped"
	OutcomeLeftAlone         = "left-alone-unidentified"
	OutcomeFailed            = "failed"
	OutcomeNothingInstalled  = "nothing-installed"
	OutcomeSpawned           = "spawned" // install/uninstall started; watch SetupProgress
	OutcomeRefusedNotLocal   = "refused-not-local"
	OutcomeRefusedNeedsPurge = "refused-confirm-purge"
)

// planeOps is the SUBSET of planebind this contract needs: the three short verbs and a status read.
//
// AN INTERFACE RATHER THAN THE CONCRETE TYPE, for two reasons. It is the whole dependency — a reader can
// see at a glance that managing an instance needs no more than these four calls. And it makes every
// outcome drivable in a test, including the ones a real plane will not produce on demand (a stop that
// leaves an unidentifiable process alone, a restart that only half-succeeded). planebind's own seams are
// unexported by design, so faking a result through it is not possible — which is right for production and
// useless for a test.
type planeOps interface {
	Status(name string) (planebind.StatusDTO, error)
	Start(name string) (planebind.ActionDTO, error)
	Stop(name string) (planebind.ActionDTO, error)
	Restart(name string) (planebind.ActionDTO, error)
}

// newPlaneService builds the production implementation. A package-level seam so a test can substitute one.
var newPlaneService = func(out interface{ Write([]byte) (int, error) }) planeOps {
	svc := &planebind.Service{}
	if out != nil {
		svc.Out = out
	}
	return svc
}

// List returns every instance on this machine, sorted by name.
//
// THE DIRECTORY IS THE RECORD — the same definition `orchicon` itself uses. An instance exists because
// its state directory exists, which is the only definition that also covers one created by hand
// (`orchicon install --instance mine`), not just the three the port table pre-books.
func List() ([]Instance, error) {
	dirs, err := instanceDirs()
	if err != nil {
		return nil, err
	}
	out := make([]Instance, 0, len(dirs))
	for _, name := range dirs {
		inst, err := Status(name)
		if err != nil {
			// ONE UNREADABLE INSTANCE MUST NOT HIDE THE OTHERS. The list is the management screen's
			// whole content; failing it wholesale because one directory is odd would leave the operator
			// with an empty page and no way to act on the ones that ARE fine.
			out = append(out, Instance{
				Name:    name,
				DataDir: instanceDir(name),
				Known:   instance.Known(name),
				Plane:   planebind.StatusDTO{Instance: name, State: "plane-error", Detail: err.Error()},
			})
			continue
		}
		out = append(out, inst)
	}
	return out, nil
}

// Status reads one instance's state. It never starts anything.
func Status(name string) (Instance, error) {
	if err := instance.ValidateName(name); err != nil {
		return Instance{}, err
	}
	dir := instanceDir(name)
	if !isDir(dir) {
		return Instance{}, fmt.Errorf("no instance named %q on this machine (no state directory at %s)", name, dir)
	}
	dto, err := newPlaneService(nil).Status(name)
	if err != nil {
		return Instance{}, fmt.Errorf("read %s's plane status: %w", name, err)
	}
	inst := Instance{
		Name:        name,
		DataDir:     dir,
		ControlPort: dto.ControlPort,
		Plane:       dto,
		Known:       instance.Known(name),
	}
	if dto.ControlPort > 0 {
		inst.URL = fmt.Sprintf("http://localhost:%d", dto.ControlPort)
	}
	// The setup record is best-effort: an unreadable one must not fail a status read.
	if p, err := ReadSetupProgress(name); err == nil {
		inst.Setup = p
	}
	return inst, nil
}

// Start brings an instance up. The outcome words come from planebind, so this is the SAME answer the
// desktop has always given — not a second opinion.
func Start(name string, opts Options) (Result, error) {
	return act(name, "start", opts, func(s planeOps) (planebind.ActionDTO, error) {
		return s.Start(name)
	})
}

// Stop takes an instance down. Idempotent: stopping what is already stopped is a success.
func Stop(name string, opts Options) (Result, error) {
	return act(name, "stop", opts, func(s planeOps) (planebind.ActionDTO, error) {
		return s.Stop(name)
	})
}

// Restart stops then starts, as one outcome.
func Restart(name string, opts Options) (Result, error) {
	return act(name, "restart", opts, func(s planeOps) (planebind.ActionDTO, error) {
		return s.Restart(name)
	})
}

// act runs one of the three short verbs through planebind and flattens its DTO.
func act(name, action string, opts Options, fn func(planeOps) (planebind.ActionDTO, error)) (Result, error) {
	if err := instance.ValidateName(name); err != nil {
		return Result{Action: action, Instance: name, Outcome: OutcomeFailed, Message: err.Error()}, err
	}
	svc := newPlaneService(opts.Out)
	dto, err := fn(svc)
	res := Result{
		Action:   action,
		Instance: name,
		Outcome:  dto.Outcome,
		OK:       dto.OK && err == nil,
		Message:  dto.Message,
		LogPath:  dto.LogPath,
	}
	if err != nil {
		res.Outcome = OutcomeFailed
		res.OK = false
		if res.Message == "" {
			res.Message = err.Error()
		}
		return res, err
	}
	if res.Outcome == "" {
		// planebind always sets one; an empty word would render as a blank row, so say the honest
		// minimum rather than invent a success.
		res.Outcome = OutcomeFailed
		res.OK = false
		res.Message = "the operation returned no outcome"
	}
	return res, nil
}

// Install sets an instance up. It SPAWNS the CLI and returns as soon as the process is started, because
// a real install downloads ~500 MB and initialises a cluster — minutes, not milliseconds.
//
// PROGRESS IS THE FILE, not this return value: the caller polls SetupProgress. That is why the result's
// outcome is OutcomeSpawned and why a spawn failure is the only error that can be reported here.
//
// A GUI MUST NOT BLOCK ON THIS. The MSI already learned that lesson from the other side: holding a
// wizard open for the download is what made setup look like a hang.
func Install(name string, opts Options) (Result, error) {
	if err := instance.ValidateName(name); err != nil {
		return Result{Action: "install", Instance: name, Outcome: OutcomeFailed, Message: err.Error()}, err
	}
	return spawn(name, "install", installArgs(name, opts), opts.Out)
}

// installArgs is the command line Install runs, split out so it can be asserted without spawning.
//
// THE ARGS ARE THE CONTRACT with the single implementation. A test that only checked "Install spawns
// something" would pass while the flags drifted; these are what the CLI actually reads.
//
// NO PORTS PINNED MEANS NO --port AT ALL, and that is load-bearing rather than tidy: an unpinned reinstall
// must leave the instance's own recorded ports alone. Passing the defaults explicitly is how an existing
// instance's database gets renumbered onto the next free port (the 5433 bug).
func installArgs(name string, opts Options) []string {
	args := []string{"install", "--non-interactive", "--instance", name}
	for _, kv := range sortedPortArgs(opts.Ports) {
		args = append(args, "--port", kv)
	}
	if opts.RuntimeImages != "" {
		args = append(args, "--runtime-images", opts.RuntimeImages)
	}
	return args
}

// Uninstall removes an instance, or every instance when name is "".
//
// IT REFUSES TO PURGE WITHOUT ConfirmPurge — not because the CLI needs it (the CLI's --purge IS the
// confirmation), but because the three callers of this function are GUIs whose job is to make an
// irreversible act deliberate. Encoding that here means neither client can forget it, and the refusal is
// a typed outcome rather than a thrown error so the UI can render it as a question.
func Uninstall(name string, opts Options) (Result, error) {
	if name != "" {
		if err := instance.ValidateName(name); err != nil {
			return Result{Action: "uninstall", Instance: name, Outcome: OutcomeFailed, Message: err.Error()}, err
		}
	}
	return spawn(name, "uninstall", uninstallArgs(name, opts), opts.Out)
}

// uninstallArgs is the command line Uninstall runs. name == "" means every instance on the machine, which
// is what the MSI's uninstall uses (see cmd/orchicon/uninstall.go for why it must be ALL rather than one).
func uninstallArgs(name string, opts Options) []string {
	args := []string{"uninstall"}
	if name == "" {
		args = append(args, "--all")
	} else {
		args = append(args, name)
	}
	if opts.Purge {
		args = append(args, "--purge")
	}
	return args
}

// spawn runs one CLI verb detached and records that it was started.
func spawn(name, action string, args []string, out interface{ Write([]byte) (int, error) }) (Result, error) {
	bin, err := planecontrol.PlaneBinary()
	if err != nil {
		// NOT A BARE ERROR: planecontrol's text already names the remedy (install `orchicon`, put it on
		// PATH), and wrapping it in "spawn install: …" would bury that.
		return Result{Action: action, Instance: name, Outcome: OutcomeFailed, Message: err.Error()}, err
	}
	cmd := newCommand(bin, args...)
	cmd.Stdout = writerOrDiscard(out)
	cmd.Stderr = writerOrDiscard(out)
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return Result{Action: action, Instance: name, Outcome: OutcomeFailed,
			Message: fmt.Sprintf("could not start `%s %s`: %v", filepath.Base(bin), strings.Join(args, " "), err)}, err
	}
	// RELEASE, so nothing waits on a minutes-long process. The record of what it is doing is the
	// progress file (written by the child), not a wait handle.
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return Result{
		Action:   action,
		Instance: name,
		Outcome:  OutcomeSpawned,
		OK:       true,
		Message:  fmt.Sprintf("`%s %s` started (pid %d) — progress is in the setup record", filepath.Base(bin), args[0], pid),
		LogPath:  SetupProgressPath(name),
	}, nil
}

// sortedPortArgs renders pinned ports as NAME=PORT in a stable order, so two installs with the same pins
// produce the same command line (a diffable message, and a reproducible bug report).
func sortedPortArgs(ports map[string]int) []string {
	keys := make([]string, 0, len(ports))
	for k := range ports {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+strconv.Itoa(ports[k]))
	}
	return out
}

// instanceMarkers are the entries that make a directory an INSTANCE rather than merely a directory that
// happens to share the naming convention.
//
// THE BUG THIS EXISTS FOR. `orchicon-desktop` is real: the desktop app keeps its WebView state there
// (`hsts-storage.sqlite`, `localstorage`, `storage`, `mediakeys`). Listing every `orchicon-*` directory made
// that appear in the TUI as a server called "desktop" — which is not a server, cannot be started, and made
// the list look wrong to the one person who knows what the names should be.
//
// ANY ONE MARKER IS ENOUGH, because a partially-created instance must not be invisible: the resolver unpacks
// into bin/, the install writes config/, the supervisor writes serve/, and postgres/blobs appear with the
// first boot. The desktop's directory has none of them.
var instanceMarkers = []string{"postgres", "serve", "blobs", "config", "ports.json"}

// isInstanceDir reports whether dir holds instance state.
func isInstanceDir(dir string) bool {
	for _, marker := range instanceMarkers {
		if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
			return true
		}
	}
	return false
}

// instanceDirs lists installed instance names from the state directories under the user's home.
func instanceDirs() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil, errors.New("cannot locate the user home directory")
	}
	entries, err := os.ReadDir(filepath.Join(home, ".local", "share"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no Orchicon has ever been installed here: an empty list, not an error
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), instanceDirPrefix) {
			continue
		}
		name := strings.TrimPrefix(e.Name(), instanceDirPrefix)
		if name == "" {
			continue
		}
		// A DIRECTORY IS NOT AN INSTANCE. `orchicon-desktop` holds the app's own WebView state; requiring
		// instance markers keeps it — and any future non-instance sibling — out of the server list.
		if !isInstanceDir(filepath.Join(home, ".local", "share", e.Name())) {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// instanceDir is where one instance keeps its state. Mirrors planecontrol's own layout so this package
// cannot disagree with the CLI about where a server lives.
func instanceDir(name string) string { return planecontrol.HostDataDirFor(name) }

// instanceDirPrefix is the state-directory prefix. Mirrors planecontrol's layout.
const instanceDirPrefix = "orchicon-"

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// setupProgressStaleAfter is when a "running" record stops being believable. The installer touches
// updated_at as it moves between phases; a gap this large means the process died rather than worked.
const setupProgressStaleAfter = 20 * time.Second

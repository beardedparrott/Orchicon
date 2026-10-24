package procwin

// procwin_test.go — the repo-wide guard for "no background operation launches a visible command prompt".
//
// THE COMPLAINT HAS NOW ARRIVED THREE TIMES:
//
//   "ORCHICON on Windows pops up a ton of command prompt windows"
//   "No, it's happening in the desktop app when you install a new server until it says setup complete"
//   "There are still random command prompts in windows that pop up every so often and go away quickly"
//
// Each earlier round fixed the call sites the report named, and the windows came back — first from the
// packages the install reaches, then from the long-lived processes doing periodic work (a version probe, a git
// read, an MCP transport starting, a `gh` call from a reconciler). A per-site fix cannot end this, because the
// set of sites grows; the rule has to be structural.
//
// SO THIS TEST WALKS THE WHOLE REPO. Any non-test file that constructs a process with a bare exec.Command or
// exec.CommandContext fails, unless it is on the short, justified allowlist below. That is the guard that makes
// the property hold for code nobody has written yet.

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// allowlist are files permitted to construct a process WITHOUT procwin, each for a stated reason.
//
// Anything else is a bug: this product's long-lived processes — the detached plane, the services supervisor,
// a detached install — have no console, and Win32 gives a console-subsystem child of a console-less process a
// BRAND NEW WINDOW.
var allowlist = map[string]string{
	// THIS PACKAGE: it is where the attribute is applied. A bare exec here is the implementation.
	"internal/procwin/procwin.go": "the package that applies the attribute — its own construction is the one site that must not call itself",

	// THE VISIBLE-CONSOLE RELAUNCH. `orchicon install` re-executes itself with CREATE_NEW_CONSOLE because the
	// MSI's custom action has no console and the setup must be able to prompt. That window is the point.
	"cmd/orchicon/console_windows.go": "deliberately creates the setup's OWN VISIBLE console so it can prompt",

	// THE PROCESS-GROUP HELPERS. These set SysProcAttr for build cancellation and construct nothing; they
	// appear here only because they mention the type.
	"internal/runtime/procattr_unix.go":    "process-group helper; the spawn it decorates goes through procwin",
	"internal/runtime/procattr_windows.go": "process-group helper; the spawn it decorates goes through procwin",
	"cmd/orchicon/procattr_unix.go":        "background/detach helper; the spawn it decorates goes through procwin",
	"cmd/orchicon/procattr_windows.go":     "background/detach helper; the spawn it decorates goes through procwin",

	// THE DELIBERATELY DETACHED DAEMONS, which cannot be helped by CREATE_NO_WINDOW and do not need it.
	//
	// These spawn a process that is meant to OUTLIVE the terminal that started it — the plane (`serve`), the
	// runtime daemon, and the setup relaunch. Their detach is set explicitly with DETACHED_PROCESS, and Win32
	// documents that CREATE_NO_WINDOW is IGNORED when combined with DETACHED_PROCESS — so adding it here would
	// be decoration that reads as a fix. What DOES matter is that everything the detached child then spawns is
	// quiet, and that is where the coverage is: the plane, the daemon and the supervisor reach procwin through
	// every one of their spawn sites (asserted by the sweep above).
	//
	// REPLACING the detach with CREATE_NO_WINDOW would give these processes a hidden console their subtree
	// inherits — strictly better in theory, and deliberately NOT done: detaching is proven to keep the plane
	// alive past a terminal close, and trading a proven property for an unverifiable improvement is how a
	// cosmetic fix becomes a regression.
	"cmd/orchicon/serve.go":                  "spawns the DETACHED plane; detach is explicit and CREATE_NO_WINDOW is ignored alongside it",
	"cmd/orchicon/install_detach_unix.go":    "spawns the DETACHED runtime daemon; same reasoning as serve.go",
	"cmd/orchicon/install_detach_windows.go": "spawns the DETACHED runtime daemon; same reasoning as serve.go",

	// OUR OWN GUI BINARY. Wails builds the desktop app with `-H windowsgui` (verified in
	// wails/pkg/commands/build/base.go), so it is a GUI-subsystem executable: Windows never creates a console
	// for it, and none can appear for it or for anything it is handed. The attribute would be a no-op.
	"cmd/orchicon/install_desktop.go":      "launches our GUI binary (windowsgui subsystem), which cannot open a console",
	"internal/desktop/relaunch_unix.go":    "launches our GUI binary; and it is the Unix half, where there is no console",
	"internal/desktop/relaunch_windows.go": "launches our GUI binary (windowsgui subsystem), which cannot open a console",
}

// TestNoBackgroundOperationSpawnsAVisibleConsole is the rule.
func TestNoBackgroundOperationSpawnsAVisibleConsole(t *testing.T) {
	root := filepath.Join("..", "..")
	inspected := 0
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" || d.Name() == "node_modules" {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				return rerr
			}
			rel = filepath.ToSlash(rel)
			if why, ok := allowlist[rel]; ok {
				_ = why
				return nil
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			body := string(b)
			inspected++
			for _, banned := range []string{"exec.Command(", "exec.CommandContext(", "osexec.Command(", "osexec.CommandContext("} {
				if i := strings.Index(body, banned); i >= 0 {
					line := 1 + strings.Count(body[:i], "\n")
					t.Errorf("%s:%d constructs a process with a bare %s.\n"+
						"This product's long-lived processes run with NO CONSOLE (the detached plane, the "+
						"services supervisor, a detached install), and Win32 gives a console-subsystem child "+
						"of a console-less process a BRAND NEW WINDOW — reported three times as \"random "+
						"command prompts that pop up and go away quickly\".\n"+
						"Use procwin.Command / procwin.CommandContext (see internal/procwin), or add this "+
						"file to the allowlist above WITH A REASON if a visible console is genuinely wanted.",
						rel, line, banned)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	// VACUITY: a walk that inspected nothing proves nothing.
	if inspected < 100 {
		t.Fatalf("only %d files inspected; the walk is not reaching the tree", inspected)
	}
}

// TestTheAttributeIsRealOnWindows checks the constant, not just the call: an attribute setter passing the
// wrong flag is worse than none, because it looks fixed.
func TestTheAttributeIsRealOnWindows(t *testing.T) {
	src, err := os.ReadFile("no_window_windows.go")
	if err != nil {
		t.Fatalf("cannot read no_window_windows.go: %v", err)
	}
	if !strings.Contains(string(src), "createNoWindow = 0x08000000") {
		t.Error("CREATE_NO_WINDOW is not defined. Without it procwin.Command is a rename, and every " +
			"background operation opens a window again")
	}
	// IT MUST COMPOSE. Several call sites set SysProcAttr themselves (a process group for build cancellation,
	// a detach for a daemon); an assignment would silently drop that and turn a cosmetic fix into a real one.
	if !strings.Contains(string(src), "CreationFlags |= createNoWindow") {
		t.Error("the attribute ASSIGNS CreationFlags instead of OR-ing into it, which silently discards a " +
			"caller's process group or detach flag")
	}
	// AND IT MUST NOT BE DETACHED_PROCESS. Detaching is what leaves a process with NO console, which is what
	// makes its own children allocate visible ones — the opposite of the intent here.
	if strings.Contains(string(src), "detachedProcess") || strings.Contains(string(src), "0x00000008") {
		t.Error("no_window_windows.go mentions DETACHED_PROCESS. detaching gives the child NO console, so its " +
			"own children each get a visible one; CREATE_NO_WINDOW gives it a HIDDEN console the whole " +
			"subtree inherits")
	}
}

func TestQuietIsSafeOnANilCommand(t *testing.T) {
	// Callers build a command conditionally; Quiet must not panic on the nil branch.
	Quiet(nil)
}

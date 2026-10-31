package runtime

// hostpaths_test.go — THE CLASS, NOT THE INSTANCE, FOR PATHS THAT CROSS INTO A CONTAINER.
//
// Six Windows failures were one mistake: a host path crossing into a Linux container unconverted. The
// allowlist compare, the -v spec, the --tmpfs dest, the entrypoint, the worktree .git pointer, and finally
// the exec cwd. Each arrived as its own report, because the set of paths crossing the boundary lived in the
// memory of whoever last edited the code.
//
// hostpaths.go moves that set onto the structs as a tag. These tests hold it there:
//
//  1. every tagged field on every request type is actually CONVERTED (behavioural, by reflection);
//  2. the two handlers that turn a request into a container command call the walker (source audit);
//  3. the specific reported failure — an exec with a Windows cwd — reaches the container in /mnt form.

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// winPaths is the host shape the reported failures used: a drive letter, an outside-$HOME project dir, and a
// worktree beneath it.
var winPaths = map[string]string{
	"Cwd":        `C:\projects\weatherapp\.orchicon-worktrees\01M69SPRKBJ5NM10W7M3V86SMH`,
	"ProjectDir": `C:\projects\weatherapp`,
}

// TestEveryTaggedHostPathIsMapped is the regression assertion.
//
// IT ENUMERATES BY REFLECTION, so a field added to any of these structs with the hostpath tag is covered
// without touching this test — and a field added to one of them WITHOUT the tag is caught by
// TestNoRequestCarriesAnUntaggedPathField below.
func TestEveryTaggedHostPathIsMapped(t *testing.T) {
	m := NewPathMapper("windows", "wsl")

	for _, tc := range []struct {
		name string
		req  any
	}{
		{"AgentRequest", &AgentRequest{}},
		{"CreateRequest", &CreateRequest{}},
		{"ExecRequest", &ExecRequest{}},
		{"StdioRequest", &StdioRequest{}},
	} {
		tagged := taggedHostPathFields(reflect.TypeOf(tc.req))
		if len(tagged) == 0 {
			t.Errorf("%s declares no orchicon:\"hostpath\" field — if it genuinely carries no host path, say "+
				"why here; otherwise a path crosses the boundary unmapped (hostpaths.go)", tc.name)
			continue
		}
		// Set every tagged field to its Windows form, then convert.
		rv := reflect.ValueOf(tc.req).Elem()
		for _, f := range tagged {
			want, ok := winPaths[f]
			if !ok {
				t.Fatalf("%s.%s is tagged as a host path but this test has no sample value for it — add one "+
					"so the conversion is actually exercised", tc.name, f)
			}
			rv.FieldByName(f).SetString(want)
		}
		mapHostPathFields(m, tc.req)

		for _, f := range tagged {
			got := rv.FieldByName(f).String()
			if strings.Contains(got, `\`) || strings.Contains(got, ":") {
				t.Errorf("%s.%s is still in HOST form after mapping: %q.\n"+
					"A Linux container cannot resolve that. In the field this is `runtime exec: chdir "+
					"C:\\...: no such file or directory` on every command, and a literal directory named for "+
					"the path appearing inside the mount.", tc.name, f, got)
			}
			if want := m.Map(winPaths[f]); got != want {
				t.Errorf("%s.%s = %q, want %q", tc.name, f, got, want)
			}
		}
	}
}

// TestNoRequestCarriesAnUntaggedPathField — the OTHER half. A new path field without the tag is invisible to
// the walker, so it crosses unmapped and the failure looks identical to the six before it.
//
// The rule is a naming convention on purpose: these structs name a host path `Cwd`, `ProjectDir`, `WorktreeDir`
// or `<something>Dir`, and every such field must say so declaratively.
func TestNoRequestCarriesAnUntaggedPathField(t *testing.T) {
	pathish := func(name string) bool {
		switch name {
		// Cwd is the one that matters most and is the one a suffix rule misses: it was THE reported field,
		// and leaving it out of this list made the audit pass while the bug was live. Measured.
		case "Dir", "Path", "File", "Root", "Cwd", "CWD", "Chdir", "Workdir":
			return true
		}
		return strings.HasSuffix(name, "Dir") || strings.HasSuffix(name, "Path")
	}
	for _, tc := range []struct {
		name string
		typ  reflect.Type
	}{
		{"AgentRequest", reflect.TypeOf(AgentRequest{})},
		{"CreateRequest", reflect.TypeOf(CreateRequest{})},
		{"ExecRequest", reflect.TypeOf(ExecRequest{})},
		{"StdioRequest", reflect.TypeOf(StdioRequest{})},
	} {
		for i := 0; i < tc.typ.NumField(); i++ {
			f := tc.typ.Field(i)
			if !pathish(f.Name) {
				continue
			}
			if f.Tag.Get(hostPathTag) != hostPathTagValue {
				t.Errorf("%s.%s looks like a path field but is not tagged orchicon:\"hostpath\", so "+
					"mapHostPathFields will never convert it — this is exactly how the exec cwd reached a "+
					"container in Windows form", tc.name, f.Name)
			}
		}
	}
}

// TestBothContainerHandlersMapTheRequest is the wiring assertion.
//
// The walker being correct is what made the original gap invisible: ResolveContainerExe and the ProjectDir
// mapping both existed and worked, and the field beside them did not. A test of the walker alone cannot catch
// a handler that never calls it.
func TestBothContainerHandlersMapTheRequest(t *testing.T) {
	b, err := os.ReadFile("daemon.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	// One call per boundary: exec, stdio, and create.
	if got := strings.Count(src, "mapHostPathFields(d.Paths, &req)"); got < 3 {
		t.Errorf("only %d call(s) to mapHostPathFields(d.Paths, &req) in daemon.go; want 3 — the exec handler, "+
			"the stdio handler and createContainer. A boundary that does not convert lets a host path into "+
			"the container.", got)
	}
	for _, fn := range []string{"handleRuntimeExec", "handleRuntimeStdio", "createContainer"} {
		i := strings.Index(src, "func (d *Daemon) "+fn+"(")
		if i < 0 {
			t.Errorf("%s is missing from daemon.go", fn)
			continue
		}
		// The call must be inside the function: from its declaration to the next top-level func.
		rest := src[i:]
		if next := strings.Index(rest[1:], "\nfunc "); next > 0 {
			rest = rest[:next+1]
		}
		if !strings.Contains(rest, "mapHostPathFields(d.Paths, &req)") {
			t.Errorf("%s never calls mapHostPathFields — a host path reaching the container from here is the "+
				"reported `chdir C:\\...: no such file or directory`", fn)
		}
	}
}

// TestIdentityModeLeavesPathsAlone — the conversion must be a no-op on Linux, where the engine shares the
// host's filesystem namespace. A "normalisation" that touched a Linux path would break every working host.
func TestIdentityModeLeavesPathsAlone(t *testing.T) {
	m := NewPathMapper("linux", "")
	req := &AgentRequest{Cwd: "/home/op/projects/app", ProjectDir: "/home/op/projects/app"}
	mapHostPathFields(m, req)
	if req.Cwd != "/home/op/projects/app" || req.ProjectDir != "/home/op/projects/app" {
		t.Errorf("identity mode rewrote a Linux path: cwd=%q projectDir=%q", req.Cwd, req.ProjectDir)
	}
}

// TestAnEmptyPathStaysEmpty — an absent field must not become "/mnt/c/" or similar. Several callers send a
// request with no cwd at all, and inventing one would point the container at a directory nobody chose.
func TestAnEmptyPathStaysEmpty(t *testing.T) {
	m := NewPathMapper("windows", "wsl")
	req := &AgentRequest{}
	mapHostPathFields(m, req)
	if req.Cwd != "" || req.ProjectDir != "" {
		t.Errorf("an unset path was invented: cwd=%q projectDir=%q", req.Cwd, req.ProjectDir)
	}
}

// TestTheReportedChdirPathNowReachesTheContainerInGuestForm is the end-to-end shape of the reported failure.
func TestTheReportedChdirPathNowReachesTheContainerInGuestForm(t *testing.T) {
	m := NewPathMapper("windows", "wsl")
	req := &ExecRequest{
		Command:    "echo hi",
		Cwd:        `C:\projects\weatherapp\.orchicon-worktrees\01M69SPRKBJ5NM10W7M3V86SMH`,
		ProjectDir: `C:\projects\weatherapp`,
	}
	mapHostPathFields(m, req)

	const wantCwd = "/mnt/c/projects/weatherapp/.orchicon-worktrees/01M69SPRKBJ5NM10W7M3V86SMH"
	if req.Cwd != wantCwd {
		t.Fatalf("cwd = %q, want %q — the container's chdir target", req.Cwd, wantCwd)
	}
	if req.ProjectDir != "/mnt/c/projects/weatherapp" {
		t.Fatalf("projectDir = %q, want /mnt/c/projects/weatherapp", req.ProjectDir)
	}
	// And nothing that crosses may retain a drive-letter colon or a backslash.
	for _, v := range []string{req.Cwd, req.ProjectDir} {
		if strings.ContainsAny(v, `\:`) {
			t.Errorf("%q is still in host form", v)
		}
	}
	_ = filepath.Separator
}

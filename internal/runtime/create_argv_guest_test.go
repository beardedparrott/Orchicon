package runtime

// create_argv_guest_test.go — A DRIVE LETTER MUST NEVER REACH THE ENGINE'S VOLUME PARSER.
//
// Reported from Windows, verbatim:
//
//	create runtime orchicon-runtime-e93bfb9…: Error: invalid container path "C", must be an absolute path
//	: exit status 125
//
// podman splits a volume spec on ":", so `-v C:\projects\weatherapp:/x` is read as source="C",
// destination="\projects\weatherapp", remainder="/x" — and the drive letter alone reaches the container-path
// validator. Exit 125, no container, nothing in the log naming the argument.
//
// The guard tests below are the boundary sweep; the parse tests reproduce podman's split so the failure mode
// itself is pinned rather than described.

import (
	"os"
	"strings"
	"testing"
)

// podmanSplit is HOW PODMAN READS A VOLUME SPEC: split on every ':', first field is the source, second the
// destination, rest are options. It reproduces the reported rejection without needing an engine.
func podmanSplit(spec string) (source, dest string, ok bool) {
	parts := strings.Split(spec, ":")
	if len(parts) < 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// TestAWindowsVolumeSpecWouldBeMisparsed is the reported bug, stated as the parser sees it.
func TestAWindowsVolumeSpecWouldBeMisparsed(t *testing.T) {
	src, dst, ok := podmanSplit(`C:\projects\weatherapp:/workspace`)
	if !ok {
		t.Fatal("spec did not split")
	}
	if src != "C" || dst != `\projects\weatherapp` {
		t.Fatalf("test is not modelling podman: got source=%q dest=%q", src, dst)
	}
	// And the source is exactly what the engine rejects as a container path.
	if strings.ContainsAny(src, `/\`) || src == "" {
		t.Fatalf("the reported error is about a bare %q; this test models something else", src)
	}
}

// TestGuestifyMakesTheSameSpecParseable is the fix, asserted through the same parser.
func TestGuestifyMakesTheSameSpecParseable(t *testing.T) {
	m := NewPathMapper("windows", "wsl")
	out, err := guestifyMountSpec(m, `C:\projects\weatherapp:/workspace`)
	if err != nil {
		t.Fatalf("guestifyMountSpec: %v", err)
	}
	src, dst, _ := podmanSplit(out)
	if src == "C" {
		t.Fatalf("source is still the bare drive letter (%q) — the engine will reject this exactly as reported", out)
	}
	if strings.Contains(src, `\`) {
		t.Fatalf("source %q still holds a backslash — not engine form", src)
	}
	if src != "/mnt/c/projects/weatherapp" {
		t.Fatalf("source = %q, want the WSL mount form", src)
	}
	if dst != "/workspace" {
		t.Fatalf("destination = %q, want it untouched", dst)
	}
}

// TestGuestifyIsIdempotent is what lets it run AFTER producers that already mapped: a guest-form spec has no
// drive letter, so there is nothing to rewrite and nothing to double-map.
func TestGuestifyIsIdempotent(t *testing.T) {
	m := NewPathMapper("windows", "wsl")
	once, err := guestifyMountSpec(m, `C:\projects\weatherapp:C:\projects\weatherapp:ro`)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := guestifyMountSpec(m, once)
	if err != nil {
		t.Fatal(err)
	}
	if once != twice {
		t.Fatalf("not idempotent:\n once=%q\ntwice=%q", once, twice)
	}
	if !strings.HasSuffix(twice, ":ro") {
		t.Fatalf("the :ro option was lost: %q", twice)
	}
}

// TestGuestifySweepsAWholeArgv keeps the guarantee at the boundary rather than per producer: ONE producer that
// forgets to map must not be able to ship a malformed mount.
func TestGuestifySweepsAWholeArgv(t *testing.T) {
	m := NewPathMapper("windows", "wsl")
	argv := []string{
		"create", "--name", "c1",
		"-v", `C:\projects\weatherapp:C:\projects\weatherapp:ro`, // a producer that forgot
		"-e", "GH_TOKEN=secret",
		"-v", "/mnt/c/already/mapped:/mnt/c/already/mapped:ro",
		"image", "entrypoint",
	}
	out, err := guestifyCreateArgv(m, argv)
	if err != nil {
		t.Fatalf("guestifyCreateArgv: %v", err)
	}
	for i, a := range out {
		if i > 0 && out[i-1] == "-v" {
			if src, _, _ := podmanSplit(a); src == "C" || strings.ContainsRune(src, '\\') {
				t.Errorf("mount %q would be misparsed by the engine", a)
			}
		}
	}
	// Non-mount arguments are untouched, including the env value.
	if !containsStr(out, "GH_TOKEN=secret") {
		t.Errorf("the -e value was rewritten or lost: %v", out)
	}
}

// TestGuestifyOnLinuxIsANoOp keeps the identity case exactly as it was — this must change nothing off Windows.
func TestGuestifyOnLinuxIsANoOp(t *testing.T) {
	m := NewPathMapper("linux", "")
	argv := []string{"create", "-v", "/home/me/proj:/home/me/proj:ro"}
	out, err := guestifyCreateArgv(m, argv)
	if err != nil {
		t.Fatal(err)
	}
	for i := range argv {
		if out[i] != argv[i] {
			t.Fatalf("arg %d changed on an identity mapper: %q -> %q", i, argv[i], out[i])
		}
	}
}

// TestDescribeCreateArgvRedactsSecrets: the argv log is the whole point of the change, so it must not become a
// credential leak. Mount arguments print in full; env VALUES do not.
func TestDescribeCreateArgvRedactsSecrets(t *testing.T) {
	got := describeCreateArgv([]string{
		"create", "-e", "GH_TOKEN=ghp_secret_value",
		"--env", "ANTHROPIC_API_KEY=sk-ant-secret",
		"-v", "/mnt/c/p:/mnt/c/p:ro",
	})
	if strings.Contains(got, "ghp_secret_value") || strings.Contains(got, "sk-ant-secret") {
		t.Fatalf("a secret reached the argv log:\n%s", got)
	}
	if !strings.Contains(got, "GH_TOKEN=") || !strings.Contains(got, "ANTHROPIC_API_KEY=") {
		t.Errorf("the KEYS should survive so the log says which variable:\n%s", got)
	}
	// The mount, which is what a path bug involves, must be printed in full.
	if !strings.Contains(got, "/mnt/c/p:/mnt/c/p:ro") {
		t.Errorf("mount arguments must be readable in the log:\n%s", got)
	}
}

// TestTheCreatePathSweepsItsArgv pins the WIRING, not the function.
//
// The tests above prove guestifyCreateArgv works; none of them can prove it is CALLED. That distinction is the
// whole failure mode being fixed — every mount producer in this package was "supposed to" map, and the one that
// did not was invisible because Linux identity makes a missing map indistinguishable from a correct one. A guard
// nothing calls is exactly that bug with a new name.
//
// It reads the source because there is no seam: createContainer's body is engine-bound (createFn short-circuits
// before the argv is built), so an in-process assertion cannot observe the sweep. Asserting that the call exists
// at the point the argv is handed to the engine is the strongest claim available here, and it fails if a refactor
// removes it.
func TestTheCreatePathSweepsItsArgv(t *testing.T) {
	b, err := os.ReadFile("daemon.go")
	if err != nil {
		t.Fatalf("read daemon.go: %v", err)
	}
	src := string(b)

	sweepAt := strings.Index(src, "args, gerr := guestifyCreateArgv(d.Paths, args)")
	if sweepAt < 0 {
		t.Fatal("createContainer no longer sweeps its argv through guestifyCreateArgv — a producer that " +
			"forgets to map would ship `-v C:\\...` to the engine, which is the reported exit-125 failure")
	}
	engineAt := strings.Index(src, "out, err = d.engine(args...)")
	if engineAt < 0 {
		t.Fatal("could not find the engine create call")
	}
	if sweepAt > engineAt {
		t.Fatal("the sweep runs AFTER the engine call — it would guard nothing")
	}
	// And the failure must name the argument vector, or the next report is undiagnosable again.
	if !strings.Contains(src, "describeCreateArgv(args)") {
		t.Error("create failures no longer include the engine argv — that omission is why the reported " +
			"failure said only `invalid container path \"C\"` with no way to see which argument was wrong")
	}
}

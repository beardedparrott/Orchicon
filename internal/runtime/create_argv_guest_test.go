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

// pathTakingFlags is EVERY create flag whose argument is a path. A new mount-ish flag added to the argv
// builder must be added here, and the tests below then cover it for free.
//
// THIS LIST IS THE POINT. The first sweep covered `-v` only, and the reported failure was a `--tmpfs`: every
// volume argument in that argv was already in guest form, so the guard that was supposed to make a producer
// unable to slip looked straight past the one argument still in host form. Enumerating the flags is what turns
// "we fixed the one we saw" into "the class is closed".
var pathTakingFlags = []string{"-v", "--volume", "--tmpfs"}

// TestEveryPathTakingFlagIsGuestified is the table-driven assertion, over the flag list rather than the call
// sites.
func TestEveryPathTakingFlagIsGuestified(t *testing.T) {
	m := NewPathMapper("windows", "wsl")
	for _, flag := range pathTakingFlags {
		t.Run(flag, func(t *testing.T) {
			argv := []string{"create", "--name", "c1", flag, `C:\Users\justi\.local\share\opencode:rw,size=2g`, "img"}
			out, err := guestifyCreateArgv(m, argv)
			if err != nil {
				t.Fatalf("%s %s was not made engine-visible: %v", flag, argv[4], err)
			}
			got := out[4]
			if hasDriveLetter(got) || strings.ContainsRune(got, '\\') {
				t.Fatalf("%s argument %q still holds a Windows path — the engine splits on ':' and reads the "+
					"drive letter as the container path", flag, got)
			}
			if !strings.HasPrefix(got, "/mnt/c/") {
				t.Fatalf("%s argument %q is not the engine-visible form", flag, got)
			}
		})
	}
}

// TestTheReportedTmpfsIsFixed is the exact argument from the report, verbatim.
func TestTheReportedTmpfsIsFixed(t *testing.T) {
	m := NewPathMapper("windows", "wsl")
	out, err := guestifyTmpfsSpec(m, `C:\Users\justi\.local\share\opencode:rw,mode=0777,size=2g`)
	if err != nil {
		t.Fatalf("guestifyTmpfsSpec: %v", err)
	}
	// podman parses --tmpfs as DEST[:OPTIONS]; the dest must be a guest path and the options must survive.
	dest, opts, _ := strings.Cut(out, ":")
	if dest != "/mnt/c/Users/justi/.local/share/opencode" {
		t.Fatalf("dest = %q, want the WSL mount form", dest)
	}
	if !strings.Contains(opts, "mode=0777") || !strings.Contains(opts, "size=2g") {
		t.Fatalf("the tmpfs options were lost: %q", opts)
	}
	// And the old form is exactly what the engine rejects.
	if d, _, _ := strings.Cut(`C:\Users\justi\.local\share\opencode:rw,mode=0777`, ":"); d != "C" {
		t.Fatal("test is not modelling podman's split")
	}
}

// TestEveryPathFlagInARealisticArgvIsGuarded runs the sweep over a vector shaped like the reported one, so the
// interaction (many already-mapped args, one not) is covered rather than each flag in isolation.
func TestEveryPathFlagInARealisticArgvIsGuarded(t *testing.T) {
	m := NewPathMapper("windows", "wsl")
	argv := []string{
		"create", "--name", "orchicon-runtime-x", "--user", "1000:1000",
		"-v", `/mnt/c/projects/weatherapp:/mnt/c/projects/weatherapp`,
		"-v", `/mnt/c/Users/justi/.config/opencode:/mnt/c/Users/justi/.config/opencode:ro`,
		"--tmpfs", `C:\Users\justi\.local\share\opencode:rw,mode=0777,size=2g`,
		"--tmpfs", `/mnt/c/Users/justi:rw,mode=0777,size=2g`,
		"-e", "HOME=/mnt/c/Users/justi",
		"image", "orchicon", "runtime-supervisor",
	}
	out, err := guestifyCreateArgv(m, argv)
	if err != nil {
		t.Fatalf("guestifyCreateArgv: %v", err)
	}
	for i, a := range out {
		if i == 0 {
			continue
		}
		switch out[i-1] {
		case "-v", "--volume", "--tmpfs":
			if hasDriveLetter(a) || strings.ContainsRune(a, '\\') {
				t.Errorf("%s %q survived the sweep", out[i-1], a)
			}
		}
	}
	// The env value is NOT a path flag and must be untouched.
	if !containsStr(out, "HOME=/mnt/c/Users/justi") {
		t.Errorf("an -e value was rewritten: %v", out)
	}
}

// TestDedupeMountsKeepsTheFirst pins the fix for the duplicate project bind in the reported argv.
//
// Not fatal, but pure noise in the vector an operator reads while debugging — and a duplicate here means the
// list is assembled from more than one place, so the NEXT duplicate could be two different SOURCES for one
// destination, which the engine resolves by order.
func TestDedupeMountsKeepsTheFirst(t *testing.T) {
	mounts := []MountSpec{
		{Source: "/p", Dest: "/p"},
		{Source: "/p", Dest: "/p"},           // the reported duplicate
		{Source: "/p/ctx", Dest: "/p/ctx"},   // distinct
		{Source: "/p", Dest: "/p", RO: true}, // same pair: a set is on (source, dest)
	}
	got := dedupeMounts(mounts)
	if len(got) != 2 {
		t.Fatalf("dedupeMounts = %v, want 2 entries", got)
	}
	if got[0].Source != "/p" || got[1].Source != "/p/ctx" {
		t.Fatalf("order not preserved: %v", got)
	}
	// FIRST WINS, so the intended (writable) mount survives rather than being replaced by a later read-only one.
	if got[0].RO {
		t.Error("the later read-only entry replaced the first — the engine would mount it read-only")
	}
}

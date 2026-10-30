package runtime

// engine_path_audit_test.go — THE CLASS, NOT THE INSTANCES.
//
// Three Windows failures in a row were the same bug wearing different clothes: a HOST path handed to the engine,
// where the engine lives in a Linux VM and needs /mnt/<drive>/…
//
//	1. the allowlist comparison  (fixed)
//	2. a `-v` volume spec        (fixed)
//	3. a `--tmpfs` dest          (fixed)
//
// Each was found by a report, one round trip at a time. That is the wrong loop, so this file AUDITS the source:
// it finds every place this package hands a path to the engine and requires each to be mapped, so the FIFTH
// instance is caught here rather than on a customer's machine.
//
// It is a source audit because the alternative is an integration test on a Windows host with a podman machine,
// which CI does not have — and the property being checked (every path argument goes through the mapper) is
// visible in the source, where a new call site is.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// engineCallRe finds an engine invocation: through the daemon's one choke point, or directly for the
// streaming/stdio paths that cannot use it.
var engineCallRe = regexp.MustCompile(`(d\.engine\(|procwin\.Command\(d\.engineBin\(\)|procwin\.CommandContext\([^,]+, d\.engineBin\(\))`)

// positionalPathRe finds the OTHER way a path reaches the engine: as a bare positional argument.
//
// `docker build -t TAG CONTEXT` names the build context with no flag at all, so a flag-only audit sails past
// exactly the shape that hid the second audited bug. These identifiers are the ones this package uses for
// directory arguments.
var positionalPathRe = regexp.MustCompile(`\b(ctxDir|contextDir|buildCtx|rootfs|bundleDir)\b`)

// pathFlagRe finds the flags whose argument is a filesystem path. Adding a member here extends the audit.
var pathFlagRe = regexp.MustCompile(`"(?:-v|--volume|--tmpfs|--mount|--workdir|-w|--rootfs)"`)

// TestNoEngineCallSitePassesAnUnmappedPathLiteral is the audit.
//
// AUDIT AT FUNCTION SCOPE, not at the call expression. The first version took a fixed window around the engine
// call and found nothing, because create's flags are assembled hundreds of lines above the call it feeds — a
// window is the wrong unit. A function that mentions a path-taking flag AND calls the engine is the unit, and
// inside it the mapper must appear.
//
// It cannot prove correctness; it flags the shape all three failures shared: a function that builds a
// path-carrying argv without consulting the mapper anywhere.
func TestNoEngineCallSitePassesAnUnmappedPathLiteral(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, rerr := os.ReadFile(f)
		if rerr != nil {
			t.Fatalf("read %s: %v", f, rerr)
		}
		for _, fn := range goFunctions(string(b)) {
			if !engineCallRe.MatchString(fn.body) {
				continue // no engine call here
			}
			if !pathFlagRe.MatchString(fn.body) && !positionalPathRe.MatchString(fn.body) {
				continue // handles no paths
			}
			checked++
			if strings.Contains(fn.body, "d.Paths.Map") ||
				strings.Contains(fn.body, "mapMounts") ||
				strings.Contains(fn.body, "mapMountArgv") ||
				strings.Contains(fn.body, "guestifyCreateArgv") ||
				strings.Contains(fn.body, "tmpfsOwned") ||
				strings.Contains(fn.body, "d.Paths.Map(") {
				continue
			}
			t.Errorf("%s: func %s() builds a PATH-carrying argv for the engine without the mapper in scope.\n"+
				"  On Windows the engine lives in a Linux VM and needs /mnt/<drive>/...; an unmapped host path "+
				"is the failure reported three times now (allowlist, -v, --tmpfs).\n"+
				"  Route the path through d.Paths.Map, or a builder that already does.", f, fn.name)
		}
	}
	if checked == 0 {
		t.Fatal("the audit found no function that both calls the engine and handles a path flag — it is " +
			"matching nothing and would pass vacuously")
	}
	t.Logf("audited %d engine-touching function(s) that handle paths", checked)
}

// goFunc is one top-level function's name and body, as a crude but dependency-free parse.
type goFunc struct {
	name string
	body string
}

// goFunctions splits a Go source file into top-level functions by brace matching.
func goFunctions(src string) []goFunc {
	var out []goFunc
	nameRe := regexp.MustCompile(`(?m)^func (?:\([^)]*\) )?([A-Za-z0-9_]+)`)
	for _, m := range nameRe.FindAllStringSubmatchIndex(src, -1) {
		open := strings.IndexByte(src[m[1]:], '{')
		if open < 0 {
			continue
		}
		start := m[1] + open
		depth, i := 0, start
		for ; i < len(src); i++ {
			switch src[i] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					i++
					goto done
				}
			}
		}
	done:
		out = append(out, goFunc{name: src[m[2]:m[3]], body: src[start:i]})
	}
	return out
}

// TestTmpfsBuilderMapsInternally pins the fix at the builder, which is what makes every caller correct without
// remembering. The reported failure was a tmpfs whose caller did not map; a builder that maps cannot be got
// wrong by its callers.
func TestTmpfsBuilderMapsInternally(t *testing.T) {
	b, err := os.ReadFile("daemon.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	i := strings.Index(src, "func (d *Daemon) tmpfsOwned(")
	if i < 0 {
		t.Fatal("tmpfsOwned is gone")
	}
	body := src[i:]
	if j := strings.Index(body, "\n}"); j > 0 {
		body = body[:j]
	}
	if !strings.Contains(body, "d.Paths.Map(path)") {
		t.Fatal("tmpfsOwned no longer maps its path — every caller must then remember to, and the reported " +
			"failure was a caller that did not")
	}
}

// TestProjectDirIsMappedBeforeTheContainerSeesIt guards the NEXT instance of this class, found by audit:
// ProjectDir travels into the supervisor as JSON and is used as the serve's cwd.
func TestProjectDirIsMappedBeforeTheContainerSeesIt(t *testing.T) {
	b, err := os.ReadFile("daemon.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	mapAt := strings.Index(src, "req.ProjectDir = d.Paths.Map(req.ProjectDir)")
	if mapAt < 0 {
		t.Fatal("req.ProjectDir is never mapped — the in-container supervisor would be handed a Windows path " +
			"as its working directory and safety-guard root")
	}
	// It must happen at the TOP of create, before the argv is built and before any request is forwarded.
	argvAt := strings.Index(src, "args = append(args, mountArgv(mapMounts")
	if argvAt < 0 {
		t.Fatal("could not find the argv build")
	}
	if mapAt > argvAt {
		t.Error("ProjectDir is mapped AFTER the argv is built — anything reading it earlier still sees the " +
			"host form")
	}
}

// TestTheBuildContextIsMapped guards the other audited call site: the only engine invocation besides create
// that hands over a filesystem path.
func TestTheBuildContextIsMapped(t *testing.T) {
	b, err := os.ReadFile("images.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if !strings.Contains(src, `"build", "-t", req.Tag, d.Paths.Map(ctxDir)`) {
		t.Fatal("the docker build context is passed unmapped — on Windows that is C:\\Users\\…\\Temp\\…, which " +
			"the podman machine cannot resolve, so the first custom-image build would fail")
	}
	// cmd.Dir must stay the HOST path: this process changes directory, not the engine.
	if !strings.Contains(src, "cmd.Dir = ctxDir") {
		t.Error("cmd.Dir should remain the host path — changing it to the guest form would break the local " +
			"process, which is running on the host")
	}
}

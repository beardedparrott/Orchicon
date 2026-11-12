package mcpsettings

// catalog_servers_test.go — every catalog entry must point at something that EXISTS.
//
// A dead entry fails late and confusingly: the operator picks it, the plane stores it, and the server
// dies on first launch with a resolution error that names a package, not a mistake in our catalog.
// Five were dead at once — fetch and time pointed at npm scopes that were never published, git at a
// SECURITY HOLDING PACKAGE (npm's "do not install" tombstone on the name "mcp-server-git"), slack at a
// Python module that does not exist on PyPI, and everart at a package and a repository that are both
// gone.

import (
	"flag"
	"os/exec"
	"strings"
	"testing"
)

// catalogLiveFlag gates the network checks. A plain -flag on `go test` needs no wiring; it is declared
// here and read through a helper so the skip message can name it.
var catalogLiveFlag = flag.Bool("catalog-live", false, "verify every catalog package still exists (network)")

func catalogLiveEnabled() bool { return *catalogLiveFlag }

// TestCatalogServerConfigsAreWellFormed is the STATIC half: cheap, always runs, and pins the exact
// mistakes that were made — a command that does not match its declared install mechanism, an empty
// arg list, a remote entry without a URL.
//
// A source assertion is the right tool for the shape; the reachability of each package is a NETWORK
// fact and lives in the opt-in test below, because CI must not fail on a vendor outage.
func TestCatalogServerConfigsAreWellFormed(t *testing.T) {
	for _, e := range catalog {
		switch e.InstallMechanism {
		case MechanismNpx:
			if e.DefaultCommand != "npx" {
				t.Errorf("%s: mechanism npx but command %q — the installer runs the command, so this cannot start", e.Slug, e.DefaultCommand)
			}
		case MechanismUvx:
			if e.DefaultCommand != "uvx" {
				t.Errorf("%s: mechanism uvx but command %q — the installer runs the command, so this cannot start", e.Slug, e.DefaultCommand)
			}
		case MechanismRemoteURL:
			if !strings.HasPrefix(e.DefaultURL, "http") {
				t.Errorf("%s: mechanism remote_url but url %q", e.Slug, e.DefaultURL)
			}
			continue
		default:
			t.Errorf("%s: unknown install mechanism %q", e.Slug, e.InstallMechanism)
		}
		if len(e.DefaultArgs) == 0 {
			t.Errorf("%s: no args — the server package is never named, so nothing resolves", e.Slug)
		}
	}
}

// TestCatalogNamesNoPackageWeKnowIsDead pins the specific tombstones, so a future edit cannot quietly
// restore one of them.
//
// IT IS MECHANISM-AWARE, which is the whole subtlety: "mcp-server-git" is an npm SECURITY HOLDING
// PACKAGE (npm's "do not install" marker on the name) and simultaneously a perfectly good PyPI package.
// The same string is dead through npx and correct through uvx, so a flat list would either miss the bug
// or forbid the fix.
func TestCatalogNamesNoPackageWeKnowIsDead(t *testing.T) {
	// name -> mechanism -> why it is dead through THAT mechanism.
	dead := map[string]map[string]string{
		"@modelcontextprotocol/server-fetch": {
			MechanismNpx: "never published on npm; the server is Python (mcp-server-fetch)",
		},
		"@modelcontextprotocol/server-time": {
			MechanismNpx: "never published on npm; the server is Python (mcp-server-time)",
		},
		"mcp-server-git": {
			MechanismNpx: "npm SECURITY HOLDING PACKAGE — a tombstone, not a server",
		},
		"mcp-server-slack": {
			MechanismUvx: "there is no mcp-server-slack on PyPI; the JS server exists",
		},
		"@everart/mcp-server": {
			MechanismNpx: "package and its repository both removed",
		},
	}
	for _, e := range catalog {
		for _, a := range e.DefaultArgs {
			byMech, known := dead[a]
			if !known {
				continue
			}
			if why, deadHere := byMech[e.InstallMechanism]; deadHere {
				t.Errorf("%s runs the dead package %q through %s (%s)", e.Slug, a, e.InstallMechanism, why)
			}
		}
	}
	// AND THE MECHANISM MUST MATCH THE ECOSYSTEM: a bare "mcp-server-*" name is a PYTHON distribution,
	// so running it through npx resolves to nothing. mcp-server-sqlite is the one JS package that
	// happens to share the naming convention.
	for _, e := range catalog {
		for _, a := range e.DefaultArgs {
			isPy := strings.HasPrefix(a, "mcp-server-")
			if isPy && e.InstallMechanism == MechanismNpx && a != "mcp-server-sqlite" {
				t.Errorf("%s runs the Python package %q through npx", e.Slug, a)
			}
		}
	}
}

// TestCatalogPackagesActuallyResolve is the LIVE half, and it is OPT-IN: it shells out to npm and
// Python metadata, so it must never fail a CI run because a registry was briefly unreachable.
//
//	go test ./internal/mcpsettings/ -run TestCatalogPackagesActuallyResolve -catalog-live
func TestCatalogPackagesActuallyResolve(t *testing.T) {
	if !catalogLiveEnabled() {
		t.Skip("network check — run with -catalog-live to verify every catalog package still exists")
	}
	for _, e := range catalog {
		if e.InstallMechanism == MechanismRemoteURL || len(e.DefaultArgs) == 0 {
			continue
		}
		switch e.InstallMechanism {
		case MechanismNpx:
			// The package is the LAST arg that is not a bare flag.
			pkg := lastPackageArg(e.DefaultArgs)
			if pkg == "" {
				t.Errorf("%s: no package argument found in %v", e.Slug, e.DefaultArgs)
				continue
			}
			out, err := exec.Command("npm", "view", pkg, "dist-tags.latest").CombinedOutput()
			if err != nil {
				t.Errorf("%s: npm cannot resolve %q: %v\n%s", e.Slug, pkg, err, out)
				continue
			}
			if strings.Contains(string(out), "security holding package") {
				t.Errorf("%s: %q is a security holding package", e.Slug, pkg)
			}
		case MechanismUvx:
			pkg := lastPackageArg(e.DefaultArgs)
			out, err := exec.Command("python3", "-c",
				"import json,urllib.request,sys; urllib.request.urlopen('https://pypi.org/pypi/'+sys.argv[1]+'/json', timeout=15)",
				pkg).CombinedOutput()
			if err != nil {
				t.Errorf("%s: PyPI has no package %q: %v\n%s", e.Slug, pkg, err, out)
			}
		}
	}
}

// lastPackageArg returns the last argument that looks like a package rather than a flag or a value
// following a flag.
func lastPackageArg(args []string) string {
	for i := len(args) - 1; i >= 0; i-- {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			continue
		}
		if i > 0 && strings.HasPrefix(args[i-1], "--from") {
			continue // the --from VALUE is the distribution, which is fine, but prefer the command
		}
		return a
	}
	return ""
}

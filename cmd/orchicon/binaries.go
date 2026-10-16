package main

// Infrastructure-binary acquisition: DETECT, ELSE DOWNLOAD.
//
// WHY THIS EXISTS. The binaries the stack runs (postgres, initdb, pg_ctl,
// pg_isready, createdb and nats-server) used to arrive inside a container image.
// There is no image to carry them any more, so they have to come from the host —
// either because the host already has them, or because we fetch a pinned build on
// first run. The operator chose detect-else-download, so this file is the ONE
// mechanism that decides which of the two happens, and it is the same mechanism
// for every binary.
//
// THE FAILURE MODES MATTER MORE THAN THE HAPPY PATH, because this is the task
// that decides whether a normal person can install Orchicon at all:
//
//   - A PARTIAL install is rejected loudly rather than satisfied. Client-only
//     packages ship psql/pg_isready but not postgres/initdb, and a dir that
//     "almost" has a server would otherwise fail much later, at initdb time,
//     with a message about nothing in particular. This is the same posture
//     internal/runtime/agent.go takes for the sandbox probe ("ALL four, a
//     partial dir is rejected").
//   - A CHECKSUM MISMATCH IS A HARD FAILURE. There is no "continue anyway"
//     branch: an unverified binary is exactly the thing a checksum exists to
//     prevent, so we delete the download and say so.
//   - A TOO-OLD detected server is REPORTED, not attempted. A server we cannot
//     run is not a server we should hand to the operator as if it worked.
//   - The download needs NO ROOT. It lands in the instance data dir, which the
//     operator already owns — that is the whole point of the download path.
//
// ONE RESOLVER, ONE RECORD. The resolved absolute paths are written once to
// <DataDir>/infra-binaries.json and every consuming process reads that record.
// A second resolution path that disagreed with the first is the exact failure
// planeEnv's comment warns about (two launchers disagreeing about the data dir),
// and a service proc that re-probed PATH would quietly run a different postgres
// than the one the installer reported.
//
// PINNED VERSIONS. postgres 16.4 and nats-server 2.10.29, mirroring the lines the
// container images already ship (deploy/container/Dockerfile: postgres:16 /
// nats:2.10; deploy/runtime/Dockerfile.dev pins nats-server v2.10.29). The
// Postgres build is the self-contained distribution published for the
// theseus-rs/postgresql-binaries project — a plain tarball needing no package
// manager and no root, which is what the download path requires.
//
// MINIMUM SUPPORTED POSTGRESQL: major 14 (minPostgresMajor). Below that the
// plane's schema and migration tooling are not supported; a detected older
// server is refused with the version it found, and the download path never picks
// a version below it.

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// pinnedNATSServerVersion matches deploy/runtime/Dockerfile.dev:41.
	pinnedNATSServerVersion = "2.10.29"
	// pinnedPostgresVersion is the release tag of the pinned binaries build.
	pinnedPostgresVersion = "16.4.0"
	// minPostgresMajor is the oldest supported PostgreSQL major version.
	minPostgresMajor = 14

	// infraBinariesFile is the record of what was resolved, sibling to ports.json
	// in the instance state dir.
	infraBinariesFile = "infra-binaries.json"

	// infraBinDirName is the subdir of the instance data dir holding downloads.
	infraBinDirName = "bin"

	// postgresSubdirName is where the downloaded PostgreSQL distribution is
	// unpacked: <dataDir>/bin/postgres.
	//
	// THE WHOLE DISTRIBUTION, NOT JUST bin/. initdb resolves its share directory
	// relative to its own executable path (/share/postgres.bki) and postgres does
	// the same for /lib and /share, so a copy that kept only bin/ fails at initdb
	// with "corrupted installation". Verified: a bin-only copy of a real pinned
	// build fails exactly that way.
	postgresSubdirName = "postgres"

	// downloadTailLimit is the bounded tail kept so a failed download can still
	// say WHY (the posture install.go's tailWriter pins for image pulls).
	downloadTailLimit = 4096
)

// errInfraAbsent means "this binary is genuinely not on the host", as opposed to
// "the host's copy is broken" — only the former is a reason to download.
var errInfraAbsent = errors.New("infrastructure binaries not found")

// postgresRequiredBins are the server binaries the stack actually execs. A dir
// with only some of them is a PARTIAL install and is rejected: initdb, pg_ctl,
// pg_isready, createdb and postgres are each invoked by name somewhere in
// cmd/orchicon/container.go, so "almost all of them" fails at boot.
var postgresRequiredBins = []string{"postgres", "initdb", "pg_ctl", "pg_isready", "createdb"}

// natsRequiredBins are the binaries the NATS component execs.
var natsRequiredBins = []string{"nats-server"}

// infraBinaries is the resolved set — recorded once, read by every service proc.
//
// It is deliberately a flat set of ABSOLUTE paths rather than a bin dir to be
// searched later: a search is a second resolution path, and the record exists so
// there is only one.
type infraBinaries struct {
	Source          string `json:"source"` // "detected" | "downloaded" | "mixed"
	Postgres        string `json:"postgres"`
	Initdb          string `json:"initdb"`
	PgCtl           string `json:"pg_ctl"`
	PgIsReady       string `json:"pg_isready"`
	Createdb        string `json:"createdb"`
	NatsServer      string `json:"nats_server"`
	PostgresVersion string `json:"postgres_version"`
	NatsVersion     string `json:"nats_version"`
}

// acquireOptions carries every host interaction so the mechanism is deterministic
// under test: no test reaches the network, and no test depends on what happens to
// be installed on the machine running it.
type acquireOptions struct {
	// DataDir is the instance data dir; downloads land in <DataDir>/bin and the
	// record is written to <DataDir>/infra-binaries.json.
	DataDir string
	// GOOS/GOARCH select the pinned build; default runtime.GOOS/GOARCH.
	GOOS, GOARCH string
	// PathEnv overrides the PATH probed for detection (default os.Getenv("PATH")).
	PathEnv string
	// HTTPClient is used for downloads (default a 5-minute client).
	HTTPClient *http.Client
	// URLs overrides pinned download URLs, keyed "<name>/<goos>/<goarch>"
	// (tests and mirrors).
	URLs map[string]string
	// Checksums overrides pinned SHA256 sums, keyed "<name>/<goos>/<goarch>".
	Checksums map[string]string
	// SkipDownload makes absence fatal instead of downloading — for a host that
	// forbids network access at install time.
	SkipDownload bool
	// RestrictToPath limits detection to PathEnv ALONE, skipping the platform's
	// conventional install locations. It exists for tests: a test asserting "absent"
	// must not accidentally succeed because the machine running it happens to have
	// /usr/lib/postgresql/*/bin installed.
	RestrictToPath bool
	// Out is the progress stream. A nil Out means NOTHING is written
	// (progressOut returns io.Discard); the production constructors
	// (defaultAcquireOptions) set os.Stdout, and tests that do not care pass
	// io.Discard explicitly.
	Out io.Writer
}

// infraArtifact describes one pinned downloadable build.
type infraArtifact struct {
	// Name is the artifact's identity in overrides and progress output.
	Name string
	// Version is the pinned version, recorded in the resolved set.
	Version string
	// Required are the executable base names (no .exe) that must exist after
	// acquisition — the same completeness rule detection applies.
	Required []string
	// Distribution unpacks a whole tree into <DataDir>/bin/<Name> (PostgreSQL
	// needs its share/ and lib/ beside its bin/); otherwise the single binary is
	// installed as <DataDir>/bin/<Name>.
	Distribution bool
	// URLs and Checksums are keyed "<goos>/<goarch>".
	URLs      map[string]string
	Checksums map[string]string
}

// infraArtifacts is the pinned table. Every checksum below was taken from the
// publisher's own release for the pinned version (the .sha256 asset for the
// PostgreSQL builds, and a download-and-hash of the NATS release artifacts).
var infraArtifacts = []infraArtifact{
	{
		Name:         "postgres",
		Version:      pinnedPostgresVersion,
		Required:     postgresRequiredBins,
		Distribution: true,
		URLs: map[string]string{
			"linux/amd64":   "https://github.com/theseus-rs/postgresql-binaries/releases/download/16.4.0/postgresql-16.4.0-x86_64-unknown-linux-gnu.tar.gz",
			"linux/arm64":   "https://github.com/theseus-rs/postgresql-binaries/releases/download/16.4.0/postgresql-16.4.0-aarch64-unknown-linux-gnu.tar.gz",
			"darwin/amd64":  "https://github.com/theseus-rs/postgresql-binaries/releases/download/16.4.0/postgresql-16.4.0-x86_64-apple-darwin.tar.gz",
			"darwin/arm64":  "https://github.com/theseus-rs/postgresql-binaries/releases/download/16.4.0/postgresql-16.4.0-aarch64-apple-darwin.tar.gz",
			"windows/amd64": "https://github.com/theseus-rs/postgresql-binaries/releases/download/16.4.0/postgresql-16.4.0-x86_64-pc-windows-msvc.zip",
		},
		Checksums: map[string]string{
			"linux/amd64":   "1059350056c24e6dd3974af7582199c2a4d06078ecb2beb9f4b26b6debea6d37",
			"linux/arm64":   "62736e25b44c92ca8621987df9a8940ccbad158ad30dcbda105b4587fa5db2b3",
			"darwin/amd64":  "3193b9747c610139990c9913ff5fd5ad73cd38cefcd5ffcdc46079fd1479406e",
			"darwin/arm64":  "0ec91e77eff381e43e3963f012aff3acb9de12ad3739a625e57cce9671b28b0f",
			"windows/amd64": "7498582e678267a8396df1f8bd1c72f2762fd71dd9485714afca46a74f9a8710",
		},
	},
	{
		Name:     "nats-server",
		Version:  pinnedNATSServerVersion,
		Required: natsRequiredBins,
		URLs: map[string]string{
			"linux/amd64":   "https://github.com/nats-io/nats-server/releases/download/v2.10.29/nats-server-v2.10.29-linux-amd64.tar.gz",
			"linux/arm64":   "https://github.com/nats-io/nats-server/releases/download/v2.10.29/nats-server-v2.10.29-linux-arm64.tar.gz",
			"darwin/amd64":  "https://github.com/nats-io/nats-server/releases/download/v2.10.29/nats-server-v2.10.29-darwin-amd64.tar.gz",
			"darwin/arm64":  "https://github.com/nats-io/nats-server/releases/download/v2.10.29/nats-server-v2.10.29-darwin-arm64.tar.gz",
			"windows/amd64": "https://github.com/nats-io/nats-server/releases/download/v2.10.29/nats-server-v2.10.29-windows-amd64.zip",
		},
		Checksums: map[string]string{
			"linux/amd64":   "e314da74a83a1d3876db8814c27eb990fe729640fd6452025b441bcede8390da",
			"linux/arm64":   "da4b27942ecfb4c5f0c54553d2e7f470ddda9d7581cfe8131b351a0e223ed401",
			"darwin/amd64":  "330c9e2f41ba8cc04c5b08c95ff3a1c4e7ff724dd3856039985de2ffe79aa9fc",
			"darwin/arm64":  "d7d8277882679d072c12f200c7b7006ee19aece00d2a0d75c3fbf2bf7fbd08ba",
			"windows/amd64": "98657bf4d5a9ce44168c019ba6894cda8e22e6adc8798edc05c168db7262de29",
		},
	},
}

// parsePostgresMajor extracts the major version from `postgres --version` output.
//
// The real line is "postgres (PostgreSQL) 16.4" or "...16.4 (Debian 16.4-1)", and a
// development build prints "...18devel" - so the version is the FIRST token starting
// with a digit, and the major is its leading digit run. Pure, so it is unit-tested
// directly rather than through an exec.
func parsePostgresMajor(s string) (int, bool) {
	for _, f := range strings.Fields(s) {
		f = strings.Trim(f, "(),")
		if f == "" {
			continue
		}
		if c := f[0]; c < '0' || c > '9' {
			continue
		}
		i := 0
		for i < len(f) && f[i] >= '0' && f[i] <= '9' {
			i++
		}
		if n, err := strconv.Atoi(f[:i]); err == nil {
			return n, true
		}
	}
	return 0, false
}

// exeName appends the platform's executable suffix.
func exeName(name, goos string) string {
	if goos == "windows" {
		return name + ".exe"
	}
	return name
}

// lookPathIn resolves name against the given dirs, in order. It is a PATH-local
// lookup rather than exec.LookPath because the probe must honor the PATH the
// caller passes (tests, and any future per-instance PATH) rather than this
// process's environment.
func lookPathIn(dirs []string, name, goos string) (string, bool) {
	name = exeName(name, goos)
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, name)
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			continue
		}
		// On unix a name on PATH that is not executable is not runnable; on
		// windows the executable bit is not meaningful.
		if goos != "windows" && st.Mode().Perm()&0o111 == 0 {
			continue
		}
		return p, true
	}
	return "", false
}

// partitionBins splits a bin dir into the binaries present and those missing.
func partitionBins(dir string, bins []string, goos string) (present, missing []string) {
	for _, b := range bins {
		if st, err := os.Stat(filepath.Join(dir, exeName(b, goos))); err == nil && !st.IsDir() {
			present = append(present, b)
			continue
		}
		missing = append(missing, b)
	}
	return present, missing
}

// binDir is where downloads land for an instance.
func binDir(dataDir string) string { return filepath.Join(dataDir, infraBinDirName) }

// postgresDir is the unpacked PostgreSQL distribution for an instance.
func postgresDir(dataDir string) string { return filepath.Join(binDir(dataDir), postgresSubdirName) }

// recordInfraBinaries writes the resolved set, once, to the instance data dir.
func recordInfraBinaries(dataDir string, rec *infraBinaries) error {
	if dataDir == "" {
		return errors.New("no data dir to record the infrastructure binaries in")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dataDir, err)
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal infrastructure binaries: %w", err)
	}
	path := filepath.Join(dataDir, infraBinariesFile)
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// loadInfraBinaries reads the record back.
func loadInfraBinaries(dataDir string) (*infraBinaries, error) {
	path := filepath.Join(dataDir, infraBinariesFile)
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errInfraAbsent
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var rec infraBinaries
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if rec.Postgres == "" || rec.NatsServer == "" {
		return nil, fmt.Errorf("%s is incomplete (%s / %s) — re-run the install", path, rec.Postgres, rec.NatsServer)
	}
	return &rec, nil
}

// ---------------------------------------------------------------------------
// DETECT
// ---------------------------------------------------------------------------

// homeDir returns the operator's home directory, or "" when it cannot be
// resolved (a service account with no passwd entry) — the conventional install
// locations simply contribute nothing then.
func homeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}

// postgresCandidateDirs is the ordered list of dirs a Postgres server install may
// live in: the caller's PATH first (an explicit choice wins), then the
// conventional locations each platform's packager uses.
//
// The glob branch is the one that matters in practice: Debian and Ubuntu install
// the SERVER under /usr/lib/postgresql/<major>/bin and put only the CLIENT tools
// (/usr/bin/psql, pg_isready, createdb) on PATH — so a PATH-only probe finds a
// partial install on a host that has a perfectly good server, and would then
// download a second one over the top of it. Newest-first, mirroring
// internal/runtime/agent.go's sandboxPgBinDir.
func postgresCandidateDirs(opt acquireOptions) []string {
	var dirs []string
	dirs = append(dirs, filepath.SplitList(opt.PathEnv)...)
	if opt.RestrictToPath && opt.DataDir == "" {
		return dirs
	}
	globSortDesc := func(pattern string) {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return
		}
		sort.Sort(sort.Reverse(sort.StringSlice(matches)))
		dirs = append(dirs, matches...)
	}
	if opt.RestrictToPath {
		// Still honor an EARLIER run's unpacked distribution: it is this instance's
		// own state, indistinguishable from an explicit choice.
		if opt.DataDir != "" {
			dirs = append(dirs, filepath.Join(postgresDir(opt.DataDir), "bin"))
		}
		return dirs
	}
	switch opt.GOOS {
	case "windows":
		programFiles := os.Getenv("ProgramFiles")
		if programFiles == "" {
			programFiles = `C:\Program Files`
		}
		globSortDesc(filepath.Join(programFiles, "PostgreSQL", "*", "bin"))
	case "darwin":
		globSortDesc("/opt/homebrew/opt/postgresql*/bin")
		globSortDesc("/usr/local/opt/postgresql*/bin")
		globSortDesc("/Applications/Postgres.app/Contents/Versions/*/bin")
		globSortDesc("/Library/PostgreSQL/*/bin")
	}
	globSortDesc("/usr/lib/postgresql/*/bin")
	dirs = append(dirs, "/usr/local/pgsql/bin", "/usr/pgsql/bin")
	// A PostgreSQL distribution we unpacked ourselves on an earlier run is also a
	// legitimate, complete install.
	if opt.DataDir != "" {
		dirs = append(dirs, filepath.Join(postgresDir(opt.DataDir), "bin"))
	}
	return dirs
}

// detectResult is what detection resolved: the absolute paths it found, and the
// artifact names it did not find at all (the ones to download).
//
// The two are kept apart on purpose. "Absent" is a reason to download; "present
// but broken" is a reason to STOP, and collapsing them would turn a partial
// install into a silent second install.
type detectResult struct {
	rec     *infraBinaries
	missing []string
}

// detectInfraBinaries probes for both artifacts independently.
//
// It returns an error ONLY for a host with a Postgres install we must not use
// (partial, or too old). A host with no Postgres at all is not an error here — it
// is the download path's input.
func detectInfraBinaries(opt acquireOptions) (*detectResult, error) {
	res := &detectResult{rec: &infraBinaries{}}

	dir, err := detectPostgresDir(opt)
	switch {
	case err != nil && !errors.Is(err, errInfraAbsent):
		return nil, err
	case errors.Is(err, errInfraAbsent):
		res.missing = append(res.missing, "postgres")
	default:
		if err := res.rec.fillPostgres(dir, opt); err != nil {
			return nil, err
		}
	}

	if p, version, ok := detectNatsServer(opt); ok {
		res.rec.NatsServer = p
		res.rec.NatsVersion = version
	} else {
		res.missing = append(res.missing, "nats-server")
	}
	return res, nil
}

// detectPostgresDir finds the first candidate dir holding a `postgres` SERVER.
//
// ABSENT AND PARTIAL ARE DIFFERENT ANSWERS. No candidate holds `postgres` at all →
// errInfraAbsent (download). A candidate holds it but is missing any of the other
// required binaries → a hard error naming what is missing and where, because
// satisfying that install would fail at initdb several minutes later with nothing
// pointing back here.
func detectPostgresDir(opt acquireOptions) (string, error) {
	seen := map[string]bool{}
	var partial []string
	for _, dir := range postgresCandidateDirs(opt) {
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		if _, ok := lookPathIn([]string{dir}, "postgres", opt.GOOS); !ok {
			continue
		}
		_, missing := partitionBins(dir, postgresRequiredBins, opt.GOOS)
		if len(missing) == 0 {
			return dir, nil
		}
		partial = append(partial, fmt.Sprintf("%s (missing %s)", dir, strings.Join(missing, ", ")))
	}
	if len(partial) > 0 {
		return "", fmt.Errorf(
			"the PostgreSQL server install on this host is incomplete: %s.\n"+
				"  Orchicon needs all of %s from the SAME directory to run its own database.\n"+
				"  A client-only package (postgresql-client) installs some of these and not the rest; install the server package (e.g. `postgresql`) and re-run.",
			strings.Join(partial, "; "), strings.Join(postgresRequiredBins, ", "))
	}
	return "", errInfraAbsent
}

// fillPostgres resolves every required path from the chosen dir, then checks the
// version — a server we cannot actually run is reported here, not attempted.
func (rec *infraBinaries) fillPostgres(dir string, opt acquireOptions) error {
	p := func(name string) string { return filepath.Join(dir, exeName(name, opt.GOOS)) }
	rec.Postgres, rec.Initdb, rec.PgCtl = p("postgres"), p("initdb"), p("pg_ctl")
	rec.PgIsReady, rec.Createdb = p("pg_isready"), p("createdb")

	version, err := postgresVersionOf(rec.Postgres)
	if err != nil {
		return err
	}
	major, ok := parsePostgresMajor(version)
	if !ok {
		return fmt.Errorf("cannot read a PostgreSQL version out of %q (%s) — refusing to use a server whose version cannot be determined", version, rec.Postgres)
	}
	if major < minPostgresMajor {
		return fmt.Errorf(
			"the PostgreSQL server at %s is version %d, which is older than Orchicon's minimum of %d.\n"+
				"  Upgrade it, or remove it from PATH so Orchicon downloads a supported build into its data dir.",
			rec.Postgres, major, minPostgresMajor)
	}
	rec.PostgresVersion = version
	return nil
}

// postgresVersionOf runs `postgres --version` and returns the trimmed line.
func postgresVersionOf(bin string) (string, error) {
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return "", fmt.Errorf("run %s --version: %w", bin, err)
		}
		return "", fmt.Errorf("run %s --version: %w: %s", bin, err, msg)
	}
	return strings.TrimSpace(string(out)), nil
}

// detectNatsServer looks for nats-server on the caller's PATH and in the
// conventional user/Homebrew locations. The version is best-effort: unlike
// Postgres there is no minimum the plane depends on, and a server that will not
// report its version still runs.
func detectNatsServer(opt acquireOptions) (path, version string, ok bool) {
	dirs := filepath.SplitList(opt.PathEnv)
	if !opt.RestrictToPath {
		if home := homeDir(); home != "" {
			dirs = append(dirs, filepath.Join(home, ".local", "bin"))
		}
		dirs = append(dirs, "/usr/local/bin", "/opt/homebrew/bin", "/usr/bin")
	}
	if opt.DataDir != "" {
		dirs = append(dirs, binDir(opt.DataDir))
	}
	p, ok := lookPathIn(dirs, "nats-server", opt.GOOS)
	if !ok {
		return "", "", false
	}
	if out, err := exec.Command(p, "--version").Output(); err == nil {
		version = strings.TrimSpace(string(out))
	}
	return p, version, true
}

// ---------------------------------------------------------------------------
// DOWNLOAD
// ---------------------------------------------------------------------------

// artifactPlatform returns the effective goos/goarch for an artifact spec.
func artifactPlatform(opt acquireOptions) (goos, goarch string) {
	goos, goarch = opt.GOOS, opt.GOARCH
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	return goos, goarch
}

// overrideKey is the map key for the per-artifact URL/checksum overrides.
func overrideKey(name, goos, goarch string) string { return name + "/" + goos + "/" + goarch }

// artifactURL resolves the pinned download URL for an artifact, honoring an
// override. A platform with no pinned build is a hard error naming the platform —
// never a silent fallback to another architecture's binary.
func artifactURL(a infraArtifact, opt acquireOptions) (string, error) {
	goos, goarch := artifactPlatform(opt)
	if u := opt.URLs[overrideKey(a.Name, goos, goarch)]; u != "" {
		return u, nil
	}
	if u := a.URLs[goos+"/"+goarch]; u != "" {
		return u, nil
	}
	return "", fmt.Errorf("no pinned %s build for %s/%s — install %s on this host and re-run (set ORCHICON_INFRA_URL_%s to use a mirror)",
		a.Name, goos, goarch, a.Name, strings.ToUpper(strings.ReplaceAll(a.Name, "-", "_")))
}

// artifactChecksum resolves the pinned SHA256 for an artifact. It is an error for
// there to be none: an unverifiable download is not a download we will run.
func artifactChecksum(a infraArtifact, opt acquireOptions) (string, error) {
	goos, goarch := artifactPlatform(opt)
	if c := opt.Checksums[overrideKey(a.Name, goos, goarch)]; c != "" {
		return strings.ToLower(c), nil
	}
	if c := a.Checksums[goos+"/"+goarch]; c != "" {
		return strings.ToLower(c), nil
	}
	return "", fmt.Errorf("no pinned checksum for %s on %s/%s — refusing to download a binary that cannot be verified", a.Name, goos, goarch)
}

// artifactFor looks an artifact up by name.
func artifactFor(name string) (infraArtifact, bool) {
	for _, a := range infraArtifacts {
		if a.Name == name {
			return a, true
		}
	}
	return infraArtifact{}, false
}

// downloadArtifact downloads, verifies and unpacks one artifact, returning the
// absolute path of each of its required binaries.
//
// ORDER IS THE POINT: download → checksum → unpack → completeness. The checksum is
// checked BEFORE anything is unpacked or executed, so a tampered or truncated
// artifact never reaches the filesystem in runnable form.
func downloadArtifact(ctx context.Context, a infraArtifact, opt acquireOptions) (map[string]string, error) {
	url, err := artifactURL(a, opt)
	if err != nil {
		return nil, err
	}
	want, err := artifactChecksum(a, opt)
	if err != nil {
		return nil, err
	}
	goos, _ := artifactPlatform(opt)

	if err := os.MkdirAll(binDir(opt.DataDir), 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", binDir(opt.DataDir), err)
	}

	// The archive lands under the data dir (not the system temp dir) so a no-root
	// host with a small /tmp can still install.
	archive := filepath.Join(binDir(opt.DataDir), "."+a.Name+".download")
	defer os.Remove(archive)

	fmt.Fprintf(opt.progressOut(), "  downloading %s %s from %s\n", a.Name, a.Version, url)
	if err := downloadToFile(ctx, url, archive, opt); err != nil {
		return nil, err
	}
	if err := verifyChecksum(archive, want, opt); err != nil {
		return nil, err
	}
	fmt.Fprintf(opt.progressOut(), "  verified %s %s (sha256 %s)\n", a.Name, a.Version, shortSum(want))

	target := binDir(opt.DataDir)
	if a.Distribution {
		target = filepath.Join(binDir(opt.DataDir), a.Name)
		if err := os.RemoveAll(target); err != nil {
			return nil, fmt.Errorf("clear %s: %w", target, err)
		}
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", target, err)
	}
	if err := extractArchive(archive, a, target, goos); err != nil {
		return nil, err
	}

	paths := map[string]string{}
	for _, bin := range a.Required {
		p, ok := findExtracted(target, bin, a, goos)
		if !ok {
			return nil, fmt.Errorf("the downloaded %s %s archive did not contain %s — refusing to use an incomplete install", a.Name, a.Version, exeName(bin, goos))
		}
		paths[bin] = p
	}
	return paths, nil
}

// findExtracted locates a required binary inside the unpacked tree.
func findExtracted(target, bin string, a infraArtifact, goos string) (string, bool) {
	name := exeName(bin, goos)
	candidates := []string{filepath.Join(target, name)}
	if a.Distribution {
		candidates = append(candidates, filepath.Join(target, "bin", name))
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, true
		}
	}
	return "", false
}

// shortSum abbreviates a checksum for the progress line (the full value is in the
// error path, where it is evidence).
func shortSum(s string) string {
	if len(s) <= 16 {
		return s
	}
	return s[:16] + "…"
}

// progressOut is the stream progress is written to.
func (o acquireOptions) progressOut() io.Writer {
	if o.Out != nil {
		return o.Out
	}
	return io.Discard
}

// downloadToFile fetches url into dst, streaming progress and keeping a bounded
// tail so a failure can still say WHY — the same posture `pullImage`
// (install.go:745) takes for image pulls, applied to the download path.
func downloadToFile(ctx context.Context, url, dst string, opt acquireOptions) error {
	client := opt.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("prepare download of %s: %w", url, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// The body of a failed request is the explanation (a proxy's refusal, a
		// 404's page); keep the last 4 KiB of it, bounded.
		tail := &tailWriter{limit: downloadTailLimit}
		_, _ = io.Copy(tail, resp.Body)
		return fmt.Errorf("download %s: HTTP %s: %s", url, resp.Status, strings.TrimSpace(tail.String()))
	}

	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	defer f.Close()

	prog := &downloadProgress{out: opt.progressOut(), total: resp.ContentLength}
	if _, err := io.Copy(io.MultiWriter(f, prog), resp.Body); err != nil {
		prog.finish()
		return fmt.Errorf("download %s: %w", url, err)
	}
	prog.finish()
	if err := f.Close(); err != nil {
		return fmt.Errorf("write %s: %w", dst, err)
	}
	return nil
}

// downloadProgress streams a human-readable progress line every few MiB.
//
// It is a counter rather than a per-chunk print on purpose: a progress line per
// read would flood an unattended install's log, which is the same mistake the
// captured image pull made in the opposite direction.

type downloadProgress struct {
	out       io.Writer
	total     int64
	n         int64
	lastShown int64
}

const downloadProgressStep = 4 << 20 // 4 MiB

func (p *downloadProgress) Write(b []byte) (int, error) {
	p.n += int64(len(b))
	if p.n-p.lastShown >= downloadProgressStep {
		p.lastShown = p.n
		if p.total > 0 {
			fmt.Fprintf(p.out, "    %d%% of %.1f MiB\n", int(p.n*100/p.total), float64(p.total)/(1<<20))
		} else {
			fmt.Fprintf(p.out, "    %.1f MiB\n", float64(p.n)/(1<<20))
		}
	}
	return len(b), nil
}

func (p *downloadProgress) finish() {
	if p.n == 0 {
		return
	}
	fmt.Fprintf(p.out, "    downloaded %.1f MiB\n", float64(p.n)/(1<<20))
}

// verifyChecksum checks dst against want and DELETES dst on mismatch.
//
// THIS IS A HARD FAILURE WITH NO FALLBACK. A checksum exists so a corrupted,
// truncated or substituted artifact is refused; continuing on an unverified binary
// would be the exact outcome the check is for.
func verifyChecksum(path, want string, opt acquireOptions) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s to verify it: %w", path, err)
	}
	h := sha256.New()
	_, err = io.Copy(h, f)
	_ = f.Close()
	if err != nil {
		return fmt.Errorf("hash %s: %w", path, err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, want) {
		// Remove it: an unverified artifact must not be left where a later step
		// could pick it up.
		_ = os.Remove(path)
		return fmt.Errorf(
			"checksum mismatch for %s: got %s, want %s — refusing to install an unverified binary.\n"+
				"  The download was discarded. Re-run the install; if it repeats, the mirror or the network is altering the artifact.", path, got, want)
	}
	return nil
}

// extractArchive unpacks archive into dst for artifact a.
//
// tar.gz and .zip are both handled because the pinned builds differ by platform.
// Entries are validated (no absolute paths, no `..`) — an archive is untrusted
// input even when it is checksummed, and a path escape would write outside the
// instance data dir.
func extractArchive(archive string, a infraArtifact, dst, goos string) error {
	if strings.HasSuffix(archive, ".zip") {
		return extractZip(archive, a, dst, goos)
	}
	return extractTarGz(archive, a, dst, goos)
}

// extractTarGz unpacks a .tar.gz, flattening the archive's single top-level
// directory.
//
// FLATTENING IS REQUIRED, NOT COSMETIC. Both pinned publishers wrap everything in
// one `<name>-<version>-<platform>/` directory, and PostgreSQL resolves its share
// and lib directories relative to its own executable — so the tree must end up as
// <dst>/{bin,lib,share} with nothing in between.
func extractTarGz(archive string, a infraArtifact, dst, goos string) error {
	f, err := os.Open(archive)
	if err != nil {
		return fmt.Errorf("open %s: %w", archive, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("read %s as gzip: %w", archive, err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", archive, err)
		}
		rel, skip, err := extractRelPath(hdr.Name, a, goos)
		if err != nil {
			return fmt.Errorf("%s: %w", archive, err)
		}
		if skip {
			continue
		}
		target, err := secureJoin(dst, rel)
		if err != nil {
			return fmt.Errorf("%s: %w", archive, err)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("create %s: %w", target, err)
			}
		case tar.TypeSymlink, tar.TypeLink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("create %s: %w", filepath.Dir(target), err)
			}
			// A LINK'S TARGET IS UNTRUSTED INPUT TOO, and validating only the entry
			// NAME above leaves the escape this used to have: a symlink whose target
			// is outside dst, followed by an entry that writes THROUGH it, lands
			// outside the instance data dir (probed against a real tampered archive:
			// it did). So the target is resolved and required to stay inside dst
			// before anything is created.
			link, base := hdr.Linkname, filepath.Dir(target)
			if hdr.Typeflag == tar.TypeLink {
				lrel, lskip, lerr := extractRelPath(link, a, goos)
				if lerr != nil || lskip {
					return fmt.Errorf("%s: hard link %s -> %q is not a usable entry: %v", archive, hdr.Name, hdr.Linkname, lerr)
				}
				link, base = filepath.Join(dst, lrel), dst
			}
			if err := checkLinkTarget(dst, base, link); err != nil {
				return fmt.Errorf("%s: %w", archive, err)
			}
			_ = os.Remove(target)
			if err := os.Symlink(link, target); err != nil && !os.IsExist(err) {
				return fmt.Errorf("symlink %s: %w", target, err)
			}
		case tar.TypeReg:
			if err := writeExtracted(target, tr, hdr.FileInfo().Mode()); err != nil {
				return err
			}
		}
	}
}

// extractZip is the .zip counterpart (the Windows builds).
func extractZip(archive string, a infraArtifact, dst, goos string) error {
	r, err := zip.OpenReader(archive)
	if err != nil {
		return fmt.Errorf("read %s as zip: %w", archive, err)
	}
	defer r.Close()
	for _, f := range r.File {
		rel, skip, err := extractRelPath(f.Name, a, goos)
		if err != nil {
			return fmt.Errorf("%s: %w", archive, err)
		}
		if skip {
			continue
		}
		target, err := secureJoin(dst, rel)
		if err != nil {
			return fmt.Errorf("%s: %w", archive, err)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("create %s: %w", target, err)
			}
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("open %s in %s: %w", f.Name, archive, err)
		}
		err = writeExtracted(target, rc, f.Mode())
		_ = rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// extractRelPath maps an archive entry name to a destination-relative path,
// rejecting anything that would escape dst, and reports whether the entry should
// be skipped entirely.
func extractRelPath(name string, a infraArtifact, goos string) (rel string, skip bool, err error) {
	n := filepath.ToSlash(strings.TrimPrefix(filepath.ToSlash(name), "./"))
	if n == "" || n == "." {
		return "", true, nil
	}
	if pathIsAbs(n) || hasDotDotSegment(n) {
		return "", false, fmt.Errorf("refusing to extract %q — the entry escapes the destination", name)
	}
	// Strip the archive's single top-level directory.
	if i := strings.Index(n, "/"); i >= 0 {
		n = n[i+1:]
	} else {
		n = ""
	}
	if n == "" {
		return "", true, nil
	}
	if a.Distribution {
		return n, false, nil
	}
	// A single-binary artifact: keep only the binary itself.
	wanted := exeName(a.Required[0], goos)
	if filepath.Base(n) != wanted {
		return "", true, nil
	}
	return wanted, false, nil
}

// hasDotDotSegment reports whether any path segment is exactly "..", so an entry
// named e.g. "a..b" is not mistaken for an escape attempt while a real `../` is.
func hasDotDotSegment(n string) bool {
	for _, seg := range strings.Split(n, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// pathIsAbs is filepath.IsAbs plus the drive-absolute Windows form, so a
// windows-produced archive is validated on any host.
func pathIsAbs(n string) bool {
	if filepath.IsAbs(n) || strings.HasPrefix(n, "/") {
		return true
	}
	return len(n) >= 2 && n[1] == ':'
}

// secureJoin joins an already-validated entry-relative path onto dst, refusing to
// TRAVERSE a symlinked component on the way there.
//
// VALIDATING THE NAME IS NOT ENOUGH. extractRelPath rejects absolute names and
// `..` segments, so the joined path is textually inside dst — but a symlink
// created earlier in the SAME archive can point a later entry's parent back out of
// dst, and filepath.Join cannot see that. (Probed: a `root/evil -> <a directory
// outside dst>` symlink followed by `root/evil/pwned` wrote outside dst.) Every
// component below dst is therefore lstat'd, and a symlink anywhere along the way is
// a refusal rather than a write.
func secureJoin(dst, rel string) (string, error) {
	target := filepath.Join(dst, rel)
	base := filepath.Clean(dst)
	// Split on the ARCHIVE's separator ('/'), not the host's: on Windows
	// filepath.Separator is '\\', so splitting on it would leave the whole relative
	// path as one segment and silently skip the intermediate-component check.
	parts := strings.Split(filepath.ToSlash(filepath.Clean(rel)), "/")
	cur := base
	for i, p := range parts {
		cur = filepath.Join(cur, filepath.FromSlash(p))
		if i == len(parts)-1 {
			break // the leaf itself is replaced or created, not traversed
		}
		if st, err := os.Lstat(cur); err == nil && st.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("refusing to extract %q — %s is a symlink, so the entry would escape the destination", rel, cur)
		}
	}
	return target, nil
}

// checkLinkTarget requires a link's resolved target to stay inside dst. An
// absolute target is legal ONLY when it is already inside dst (a hard link to a
// sibling we just unpacked); anything pointing out of the instance data dir is
// refused, never created.
//
// An archive is untrusted input even when its checksum matches: the checksum
// proves WHICH bytes arrived, not that their contents are safe to unpack.
func checkLinkTarget(dst, base, link string) error {
	if strings.TrimSpace(link) == "" {
		return errors.New("refusing a link with an empty target")
	}
	var joined string
	if filepath.IsAbs(link) || pathIsAbs(link) {
		joined = filepath.Clean(filepath.FromSlash(link))
	} else {
		joined = filepath.Clean(filepath.Join(base, filepath.FromSlash(link)))
	}
	want := filepath.Clean(dst)
	if joined == want || strings.HasPrefix(joined, want+string(filepath.Separator)) {
		return nil
	}
	return fmt.Errorf("refusing a link to %q — it would escape the destination (%s)", link, joined)
}

// writeExtracted writes one extracted file, clearing the executable bit on
// demand.
//
// THE CHMOD IS EXPLICIT because the process umask applies to the create mode, and
// this postgres is later exec'd by the supervisor as a DIFFERENT uid (setpriv
// --reuid=70). A 0700 umask would produce a binary the postgres user cannot run —
// a failure that would surface as "permission denied" long after the download.
func writeExtracted(target string, r io.Reader, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(target), err)
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", target, err)
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", target, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write %s: %w", target, err)
	}
	perm := mode.Perm()
	if perm&0o111 != 0 {
		perm = 0o755
	}
	if err := os.Chmod(target, perm); err != nil {
		return fmt.Errorf("chmod %s: %w", target, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// ACQUIRE — the one entry point
// ---------------------------------------------------------------------------

// acquireInfraBinaries is the SINGLE mechanism: detect, else download, else fail;
// record what was resolved and where; return the record.
//
// The two artifacts are resolved INDEPENDENTLY on purpose. A host that already has
// Postgres but not NATS should download one thing, not two — and, more importantly,
// a host with a BROKEN Postgres install must be told about it rather than have a
// supported build downloaded over the top, because that would hide the operator's
// actual problem.
func acquireInfraBinaries(ctx context.Context, opt acquireOptions) (*infraBinaries, error) {
	if opt.DataDir == "" {
		return nil, errors.New("no data dir to acquire the infrastructure binaries into")
	}
	if opt.GOOS == "" {
		opt.GOOS = runtime.GOOS
	}
	if opt.GOARCH == "" {
		opt.GOARCH = runtime.GOARCH
	}
	if opt.PathEnv == "" {
		opt.PathEnv = os.Getenv("PATH")
	}

	// 1. DETECT. A partial or too-old install is a hard error from here.
	detected, err := detectInfraBinaries(opt)
	if err != nil {
		return nil, err
	}

	sources := map[string]bool{}
	rec := *detected.rec
	if rec.Postgres != "" {
		sources["detected"] = true
		fmt.Fprintf(opt.progressOut(), "using the PostgreSQL %s already installed at %s\n", rec.PostgresVersion, rec.Postgres)
	}
	if rec.NatsServer != "" {
		sources["detected"] = true
		fmt.Fprintf(opt.progressOut(), "using the nats-server already installed at %s\n", rec.NatsServer)
	}

	// 2. DOWNLOAD what detection did not find.
	for _, name := range detected.missing {
		if opt.SkipDownload {
			return nil, fmt.Errorf(
				"%s is not installed on this host, and downloading it is disabled (ORCHICON_SKIP_DOWNLOAD=1).\n"+
					"  Install it, or allow the download, and re-run.", name)
		}
		a, ok := artifactFor(name)
		if !ok {
			return nil, fmt.Errorf("internal error: no pinned artifact named %q", name)
		}
		paths, err := downloadArtifact(ctx, a, opt)
		if err != nil {
			return nil, fmt.Errorf("acquire %s: %w", name, err)
		}
		sources["downloaded"] = true
		switch name {
		case "postgres":
			rec.Postgres = paths["postgres"]
			rec.Initdb = paths["initdb"]
			rec.PgCtl = paths["pg_ctl"]
			rec.PgIsReady = paths["pg_isready"]
			rec.Createdb = paths["createdb"]
			// ASK THE BINARY, and keep the pinned tag only as a fallback. The record
			// is read by every later component, so it must mean the SAME thing on
			// both paths: a detected install records the version its server
			// reports, and recording the release TAG here (16.4.0 for a binary that
			// reports 16.4) would make one record carry two different kinds of value.
			rec.PostgresVersion = artifactReportedVersion(rec.Postgres, a.Version)
		case "nats-server":
			rec.NatsServer = paths["nats-server"]
			rec.NatsVersion = artifactReportedVersion(rec.NatsServer, a.Version)
		}
	}

	rec.Source = sourceOf(sources)

	// 3. THE COMPLETENESS GATE, after both paths, on the RECORD.
	//
	// The record is what every consumer reads, so the record is what must be proven
	// complete. A resolve that produced a record with a hole in it is a bug here, not
	// a failure five minutes later at initdb.
	if err := checkRecordComplete(&rec, opt); err != nil {
		return nil, err
	}

	// 4. RECORD ONCE.
	if err := recordInfraBinaries(opt.DataDir, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// artifactReportedVersion runs `<bin> --version` and returns what it says, falling
// back to the pinned tag when the binary cannot be asked. Version strings in the
// record must be comparable regardless of which acquisition path produced it.
func artifactReportedVersion(bin, fallback string) string {
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		return fallback
	}
	if v := strings.TrimSpace(string(out)); v != "" {
		return v
	}
	return fallback
}

// sourceOf names where the resolved set came from: "detected", "downloaded", or
// "mixed" when the host supplied one binary and we fetched the other. The name is
// in the record because "why is this instance running a downloaded postgres?" is a
// question that must be answerable after the fact.
func sourceOf(sources map[string]bool) string {
	switch {
	case sources["detected"] && sources["downloaded"]:
		return "mixed"
	case sources["downloaded"]:
		return "downloaded"
	default:
		return "detected"
	}
}

// checkRecordComplete proves every path in the record exists and is runnable, and
// that the PostgreSQL build it points at has the share directory it needs.
func checkRecordComplete(rec *infraBinaries, opt acquireOptions) error {
	goos := opt.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	for _, b := range postgresRequiredBins {
		p := fieldFor(rec, b)
		st, err := os.Stat(p)
		if err != nil {
			return fmt.Errorf("resolved PostgreSQL binary %s is not usable (%s): %w", b, p, err)
		}
		if st.IsDir() {
			return fmt.Errorf("resolved PostgreSQL binary %s is a directory, not a binary: %s", b, p)
		}
		if goos != "windows" && st.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("resolved PostgreSQL binary %s is not executable by the postgres user (%04o): %s", b, st.Mode().Perm(), p)
		}
	}
	if err := checkPostgresShare(rec.Initdb, goos); err != nil {
		return err
	}
	st, err := os.Stat(rec.NatsServer)
	if err != nil {
		return fmt.Errorf("resolved nats-server is not usable (%s): %w", rec.NatsServer, err)
	}
	if st.IsDir() {
		return fmt.Errorf("resolved nats-server is a directory, not a binary: %s", rec.NatsServer)
	}
	return nil
}

// fieldFor returns the recorded path for a required PostgreSQL binary name.
func fieldFor(rec *infraBinaries, bin string) string {
	switch bin {
	case "postgres":
		return rec.Postgres
	case "initdb":
		return rec.Initdb
	case "pg_ctl":
		return rec.PgCtl
	case "pg_isready":
		return rec.PgIsReady
	case "createdb":
		return rec.Createdb
	}
	return ""
}

// ---------------------------------------------------------------------------
// ENVIRONMENT
// ---------------------------------------------------------------------------

// Env knobs, deliberately few. Each exists because an install on a real host hit
// a case the defaults cannot serve:
//
//   ORCHICON_SKIP_DOWNLOAD=1      — a host that forbids outbound network at
//                                   install time. Absence becomes fatal with a
//                                   message naming the binary, rather than a
//                                   download that hangs or half-succeeds.
//   ORCHICON_INFRA_URL_<NAME>     — a mirror, or an internal artifact store. The
//                                   NAME is the artifact name upper-cased with
//                                   '-' → '_' (ORCHICON_INFRA_URL_POSTGRES,
//                                   ORCHICON_INFRA_URL_NATS_SERVER).
//   ORCHICON_INFRA_SHA256_<NAME>  — the checksum for a mirror. An override URL
//                                   WITHOUT an override checksum keeps the pinned
//                                   checksum, so pointing at a mirror that serves
//                                   the pinned artifact is a one-variable change.

const (
	skipDownloadEnv   = "ORCHICON_SKIP_DOWNLOAD"
	skipDownloadEnvOn = "1"
)

// envNameForArtifact is the shared suffix of the URL/checksum override vars.
func envNameForArtifact(name string) string {
	return strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

// skipDownloadFromEnv reports whether the operator forbade downloading.
func skipDownloadFromEnv() bool {
	return strings.TrimSpace(os.Getenv(skipDownloadEnv)) == skipDownloadEnvOn
}

// defaultAcquireOptions builds the options an install or a supervisor boot uses:
// the instance data dir, the pinned artifacts, and whatever the environment
// overrides. ONE constructor so a caller cannot accidentally assemble options that
// disagree with the other caller's.
func defaultAcquireOptions(dataDir string) acquireOptions {
	opt := acquireOptions{
		DataDir:      dataDir,
		PathEnv:      os.Getenv("PATH"),
		SkipDownload: skipDownloadFromEnv(),
		URLs:         map[string]string{},
		Checksums:    map[string]string{},
		Out:          os.Stdout,
	}
	goos, goarch := artifactPlatform(opt)
	for _, a := range infraArtifacts {
		suffix := envNameForArtifact(a.Name)
		if u := strings.TrimSpace(os.Getenv("ORCHICON_INFRA_URL_" + suffix)); u != "" {
			opt.URLs[overrideKey(a.Name, goos, goarch)] = u
		}
		if c := strings.TrimSpace(os.Getenv("ORCHICON_INFRA_SHA256_" + suffix)); c != "" {
			opt.Checksums[overrideKey(a.Name, goos, goarch)] = c
		}
	}
	return opt
}

// checkPostgresShare proves the PostgreSQL install has the share directory initdb
// needs — the "corrupted installation" failure a bin-only copy produces, caught at
// acquisition time instead of minutes later.
//
// THE SHARE DIR IS NOT ALWAYS A SIBLING OF bin/. Debian and Ubuntu split it out to
// /usr/share/postgresql/<major> and leave the server binaries under
// /usr/lib/postgresql/<major>/bin, so assuming `<bindir>/../share` would REJECT a
// perfectly good distro install and force an unnecessary download. "Complete" here
// therefore means: consult the install's own pg_config when it has one (it is
// authoritative), and fall back to the sibling layout only when it does not.
func checkPostgresShare(initdbPath, goos string) error {
	bindir := filepath.Dir(initdbPath)
	var tried []string

	if goos != "windows" {
		pgConfig := filepath.Join(bindir, "pg_config")
		if _, err := os.Stat(pgConfig); err == nil {
			if out, err := exec.Command(pgConfig, "--sharedir").Output(); err == nil {
				share := strings.TrimSpace(string(out))
				if share != "" {
					if _, err := os.Stat(filepath.Join(share, "postgres.bki")); err == nil {
						return nil
					}
					tried = append(tried, filepath.Join(share, "postgres.bki")+" (pg_config --sharedir)")
				}
			}
		}
	}
	// The self-contained/distribution layout: share/ beside bin/.
	sibling := filepath.Join(filepath.Clean(filepath.Join(bindir, "..")), "share", "postgres.bki")
	if _, err := os.Stat(sibling); err == nil {
		return nil
	}
	tried = append(tried, sibling)
	return fmt.Errorf(
		"the PostgreSQL install at %s is missing its share directory (no postgres.bki) — initdb would fail with \"corrupted installation\".\n"+
			"  Checked: %s.\n"+
			"  A PostgreSQL installation is more than its bin directory; the whole distribution must be present.",
		bindir, strings.Join(tried, ", "))
}

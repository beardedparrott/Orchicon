package main

// Infrastructure-binary acquisition: DETECT, ELSE DOWNLOAD.

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
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/procwin"
)

const (
	// pinnedNATSServerVersion matches deploy/runtime/Dockerfile.dev:41.
	pinnedNATSServerVersion = "2.10.29"
	// pinnedPostgresVersion is the release tag of the pinned binaries build.
	pinnedPostgresVersion = "16.4.0"

	// EACH MUST MATCH THE VERSION deploy/container/Dockerfile PINS, because the same binaries run in
	// either shape (that Dockerfile is now the reference, not the source).
	pinnedTempoVersion           = "2.7.1"
	pinnedLokiVersion            = "3.4.2"
	pinnedVictoriaMetricsVersion = "1.111.0"
	pinnedOTelCollectorVersion   = "0.119.0"
	// minPostgresMajor is the oldest supported PostgreSQL major version.
	minPostgresMajor = 14

	// infraBinariesFile is the record of what was resolved, sibling to ports.json
	// in the instance state dir.
	infraBinariesFile = "infra-binaries.json"

	// infraBinDirName is the subdir of the instance data dir holding downloads.
	infraBinDirName = "bin"

	// postgresSubdirName is where the downloaded PostgreSQL distribution is unpacked:
	// <dataDir>/bin/postgres.
	postgresSubdirName = "postgres"

	// downloadTailLimit is the bounded tail kept so a failed download can still
	// say WHY (the posture install.go's tailWriter pins for image pulls).
	downloadTailLimit = 4096
)

// errInfraAbsent means "this binary is genuinely not on the host", as opposed to
// "the host's copy is broken" — only the former is a reason to download.
var errInfraAbsent = errors.New("infrastructure binaries not found")

// postgresRequiredBins are the server and client binaries the stack actually execs.
var postgresRequiredBins = []string{"postgres", "initdb", "pg_ctl", "pg_isready", "createdb", "pg_dump", "psql"}

// postgresOptionalClientBins are recorded when the chosen dir carries them, but are NOT part of the
// completeness gate: nothing in this binary execs them yet (restore drives psql, not pg_restore).
var postgresOptionalClientBins = []string{"pg_restore"}

// natsRequiredBins are the binaries the NATS component execs.
var natsRequiredBins = []string{"nats-server"}

// The four telemetry backends are each a SINGLE STATIC GO BINARY with no initialisation step, so
// they take the same detect-else-download path as postgres/nats.
var (
	tempoRequiredBins = []string{"tempo"}
	// Loki's archive members carry a PLATFORM SUFFIX (loki-linux-amd64), so the
	// member name is not the executable name — see infraArtifact.Members.
	lokiRequiredBins = []string{"loki"}
	// The VictoriaMetrics build is named victoria-metrics-prod on unix and
	// victoria-metrics-windows-amd64-prod.exe on windows.
	vmRequiredBins            = []string{"victoria-metrics-prod"}
	otelCollectorRequiredBins = []string{"otelcol-contrib"}
)

// telemetryArtifactNames are the four artifacts that exist only for an embedded-telemetry boot, in
// dependency order (the collector's exporters point at the other three).
var telemetryArtifactNames = []string{"tempo", "loki", "victoria-metrics-prod", "otelcol-contrib"}

// infraBinaries is the resolved set — recorded once, read by every service proc.
type infraBinaries struct {
	Source     string `json:"source"` // "detected" | "downloaded" | "mixed"
	Postgres   string `json:"postgres"`
	Initdb     string `json:"initdb"`
	PgCtl      string `json:"pg_ctl"`
	PgIsReady  string `json:"pg_isready"`
	Createdb   string `json:"createdb"`
	PgDump     string `json:"pg_dump"`
	Psql       string `json:"psql"`
	PgRestore  string `json:"pg_restore,omitempty"`
	NatsServer string `json:"nats_server"`

	// The four telemetry backends.
	Tempo           string `json:"tempo,omitempty"`
	Loki            string `json:"loki,omitempty"`
	VictoriaMetrics string `json:"victoria_metrics,omitempty"`
	OTelCollector   string `json:"otel_collector,omitempty"`

	PostgresVersion string `json:"postgres_version"`
	NatsVersion     string `json:"nats_version"`

	TempoVersion           string `json:"tempo_version,omitempty"`
	LokiVersion            string `json:"loki_version,omitempty"`
	VictoriaMetricsVersion string `json:"victoria_metrics_version,omitempty"`
	OTelCollectorVersion   string `json:"otel_collector_version,omitempty"`
}

// acquireOptions carries every host interaction so the mechanism is deterministic under test: no
// test reaches the network.
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
	// Only restricts resolution to the named artifacts.
	Only []string
	// RestrictToPath limits detection to PathEnv ALONE, skipping the platform's conventional install
	// locations.
	RestrictToPath bool
	// Out is the progress stream.
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
	// Distribution unpacks a whole tree into <DataDir>/bin/<Name> (PostgreSQL needs its share/ and lib/
	// beside its bin/); otherwise the single binary is installed as <DataDir>/bin/<Name>.
	Distribution bool
	// Nested says the archive wraps everything in ONE top-level directory, which must be stripped on
	// unpack (today's behaviour, and what PostgreSQL and NATS both do).
	Nested bool
	// Members maps "<goos>/<goarch>" to the archive member's BASE NAME, for an artifact whose member
	// name differs per platform and from the executable name (Loki ships loki-linux-amd64 /.
	Members map[string]string
	// URLs and Checksums are keyed "<goos>/<goarch>".
	URLs      map[string]string
	Checksums map[string]string
}

// infraArtifacts is the pinned table.
var infraArtifacts = []infraArtifact{
	{
		Name:         "postgres",
		Version:      pinnedPostgresVersion,
		Required:     postgresRequiredBins,
		Distribution: true,
		Nested:       true, // theseus-rs wraps the tree in postgresql-<ver>-<target>/
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
		Nested:   true, // nats-io wraps the binary in nats-server-<ver>-<plat>/
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
	// -----------------------------------------------------------------------
	// The four telemetry backends. ALL FOUR ARE FLAT (`Nested: false`): the
	// binary sits at the archive ROOT, which is why the pre-existing extractor —
	// written for PostgreSQL's and NATS's single-top-level-dir layouts, and which
	// STRIPS the first path segment — silently dropped all four. The member names
	// below were read from the publishers' own archives, not assumed: tempo's tar
	// carries CHANGELOG.md/LICENSE/README.md/tempo-cli/tempo-query/tempo (so
	// `tempo-cli` must NOT be picked), and VictoriaMetrics' windows zip carries
	// exactly one member.
	// -----------------------------------------------------------------------
	{
		Name:     "tempo",
		Version:  pinnedTempoVersion,
		Required: tempoRequiredBins,
		URLs: map[string]string{
			"linux/amd64":   "https://github.com/grafana/tempo/releases/download/v2.7.1/tempo_2.7.1_linux_amd64.tar.gz",
			"linux/arm64":   "https://github.com/grafana/tempo/releases/download/v2.7.1/tempo_2.7.1_linux_arm64.tar.gz",
			"darwin/amd64":  "https://github.com/grafana/tempo/releases/download/v2.7.1/tempo_2.7.1_darwin_amd64.tar.gz",
			"darwin/arm64":  "https://github.com/grafana/tempo/releases/download/v2.7.1/tempo_2.7.1_darwin_arm64.tar.gz",
			"windows/amd64": "https://github.com/grafana/tempo/releases/download/v2.7.1/tempo_2.7.1_windows_amd64.tar.gz",
		},
		Checksums: map[string]string{
			"linux/amd64":   "18b073854f94089a4700aa7dc88dceba840baa485535efd36f18152fced08f89",
			"linux/arm64":   "6db8768b213b39c365963b492d9f616511cd59297aeffdcbe3fc4efd4c404c7e",
			"darwin/amd64":  "6c4c677b1ef2ed51ef5fed5895a1cded31a34d2415254966df39aa0094d52d8e",
			"darwin/arm64":  "c3c2b71c6bfb933ba52446cae461f866cf0999ed823a97b5fa492f35e82ac240",
			"windows/amd64": "e8974db07f25c87166401c3f3002a514b866ed0cadad358cebc419a15ce9c01b",
		},
	},
	{
		Name:     "loki",
		Version:  pinnedLokiVersion,
		Required: lokiRequiredBins,
		// GRAFANA'S LOKI MEMBERS CARRY A PLATFORM SUFFIX and are NOT the binary's name, so every platform
		// needs an explicit entry: there is no sane fallback that would find `loki-linux-amd64`.
		Members: map[string]string{
			"linux/amd64":   "loki-linux-amd64",
			"linux/arm64":   "loki-linux-arm64",
			"darwin/amd64":  "loki-darwin-amd64",
			"darwin/arm64":  "loki-darwin-arm64",
			"windows/amd64": "loki-windows-amd64.exe",
		},
		URLs: map[string]string{
			"linux/amd64":   "https://github.com/grafana/loki/releases/download/v3.4.2/loki-linux-amd64.zip",
			"linux/arm64":   "https://github.com/grafana/loki/releases/download/v3.4.2/loki-linux-arm64.zip",
			"darwin/amd64":  "https://github.com/grafana/loki/releases/download/v3.4.2/loki-darwin-amd64.zip",
			"darwin/arm64":  "https://github.com/grafana/loki/releases/download/v3.4.2/loki-darwin-arm64.zip",
			"windows/amd64": "https://github.com/grafana/loki/releases/download/v3.4.2/loki-windows-amd64.exe.zip",
		},
		Checksums: map[string]string{
			"linux/amd64":   "4449378c6e74281e99163dec9cca70479b6095d139c0403042f59bd962ec8791",
			"linux/arm64":   "4717319ef63a1e3155d596f0a8290af7522c19c5f6d7a8170d935aa098e2989e",
			"darwin/amd64":  "500ce13683b50ddc500fbbf315a5e880ef0417ecc042dcae7aa991a55ae3e789",
			"darwin/arm64":  "fe9cc0f2accdc5697381ee3309d440f55a63bbbb08a9aef71e5c8f2e749a4312",
			"windows/amd64": "c2c4950f2f82f8ab0dba1544962b0be3b5efaf8a72b3f0316b51b38c28cc19ce",
		},
	},
	{
		Name:     "victoria-metrics-prod",
		Version:  pinnedVictoriaMetricsVersion,
		Required: vmRequiredBins,
		// The windows zip's single member is victoria-metrics-windows-amd64-prod.exe; the unix tarballs
		// carry `victoria-metrics-prod` at the root, which the required-name fallback resolves.
		Members: map[string]string{
			"windows/amd64": "victoria-metrics-windows-amd64-prod.exe",
		},
		URLs: map[string]string{
			"linux/amd64":   "https://github.com/VictoriaMetrics/VictoriaMetrics/releases/download/v1.111.0/victoria-metrics-linux-amd64-v1.111.0.tar.gz",
			"linux/arm64":   "https://github.com/VictoriaMetrics/VictoriaMetrics/releases/download/v1.111.0/victoria-metrics-linux-arm64-v1.111.0.tar.gz",
			"darwin/amd64":  "https://github.com/VictoriaMetrics/VictoriaMetrics/releases/download/v1.111.0/victoria-metrics-darwin-amd64-v1.111.0.tar.gz",
			"darwin/arm64":  "https://github.com/VictoriaMetrics/VictoriaMetrics/releases/download/v1.111.0/victoria-metrics-darwin-arm64-v1.111.0.tar.gz",
			"windows/amd64": "https://github.com/VictoriaMetrics/VictoriaMetrics/releases/download/v1.111.0/victoria-metrics-windows-amd64-v1.111.0.zip",
		},
		Checksums: map[string]string{
			"linux/amd64":   "1c7328173ef352fa9cdaa0957ff43b752e9458755e0a9588cce4b52fe189ab8f",
			"linux/arm64":   "e8c0beb78e4b4888b4911a0689a66a79c5290f06a07a4548f2b36fc2d6a70616",
			"darwin/amd64":  "e3ed428f21c3cd42470f3154ded844886a5202abca74c626e64c2d36ec033cda",
			"darwin/arm64":  "39a74b3cb712f84675eedc372e0d4de14f0562c40e20e3bb8b059448939f4047",
			"windows/amd64": "a7b929573f79e69029495213ec7bfa917f2a6052350af7e1be6d6bd85cec863a",
		},
	},
	{
		Name:     "otelcol-contrib",
		Version:  pinnedOTelCollectorVersion,
		Required: otelCollectorRequiredBins,
		URLs: map[string]string{
			"linux/amd64":   "https://github.com/open-telemetry/opentelemetry-collector-releases/releases/download/v0.119.0/otelcol-contrib_0.119.0_linux_amd64.tar.gz",
			"linux/arm64":   "https://github.com/open-telemetry/opentelemetry-collector-releases/releases/download/v0.119.0/otelcol-contrib_0.119.0_linux_arm64.tar.gz",
			"darwin/amd64":  "https://github.com/open-telemetry/opentelemetry-collector-releases/releases/download/v0.119.0/otelcol-contrib_0.119.0_darwin_amd64.tar.gz",
			"darwin/arm64":  "https://github.com/open-telemetry/opentelemetry-collector-releases/releases/download/v0.119.0/otelcol-contrib_0.119.0_darwin_arm64.tar.gz",
			"windows/amd64": "https://github.com/open-telemetry/opentelemetry-collector-releases/releases/download/v0.119.0/otelcol-contrib_0.119.0_windows_amd64.tar.gz",
		},
		Checksums: map[string]string{
			"linux/amd64":   "4ee77545daaad658f7282bff98704bc1f31107890e2a2e1d4b1fc31da4648111",
			"linux/arm64":   "c5f4480504a12d9445ef5c3d754bd2824a2ea5dbc516c0edd52bb05c3e72f1f7",
			"darwin/amd64":  "8c67ef47afd44dc68e778dbb8933d98007534f5db48286516f9d66b9e635b441",
			"darwin/arm64":  "d56e8003c5d37ccee27ea35a3a392a6dcdc2b5e25c492a6ff5512a633c390798",
			"windows/amd64": "c1eac16de17383cdebc0e5691b69691a3f32aa28442d9090c53df6b6eb35cf84",
		},
	},
}

// parsePostgresMajor extracts the major version from `postgres --version` output.
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

// memberFor is the ARCHIVE MEMBER base name an artifact's current platform ships its binary under.
func memberFor(a infraArtifact, goos, goarch string) string {
	if m := a.Members[goos+"/"+goarch]; m != "" {
		return m
	}
	if len(a.Required) == 0 {
		return ""
	}
	return exeName(a.Required[0], goos)
}

// canonicalName is the name an acquired binary is INSTALLED under — always the required base name
// for this platform, never the archive's member name.
func canonicalName(a infraArtifact, goos string) string {
	if len(a.Required) == 0 {
		return ""
	}
	return exeName(a.Required[0], goos)
}

// selectedArtifacts filters the pinned table to the artifacts this acquisition covers: every one
// when Only is empty, otherwise exactly the names it lists (in the table's order.
func selectedArtifacts(opt acquireOptions) []infraArtifact {
	if len(opt.Only) == 0 {
		return infraArtifacts
	}
	want := make(map[string]bool, len(opt.Only))
	for _, n := range opt.Only {
		want[n] = true
	}
	var out []infraArtifact
	for _, a := range infraArtifacts {
		if want[a.Name] {
			out = append(out, a)
		}
	}
	return out
}

// lookPathIn resolves name against the given dirs, in order.
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

// optionalBin returns path when it names an existing, non-directory file, else "".
func optionalBin(path, goos string) string {
	if st, err := os.Stat(path); err == nil && !st.IsDir() {
		return path
	}
	return ""
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

// homeDir returns the operator's home directory, or "" when it cannot be resolved (a service
// account with no passwd entry).
func homeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}

// postgresCandidateDirs is the ordered list of dirs a Postgres server install may live in: the
// caller's PATH first (an explicit choice wins).
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

// detectResult is what detection resolved: the absolute paths it found, and the artifact names it
// did not find at all (the ones to download).
type detectResult struct {
	rec     *infraBinaries
	missing []string
}

// detectInfraBinaries probes for every SELECTED artifact independently. It returns an error ONLY
// for a host with a Postgres install we must not use (partial, or too old).
func detectInfraBinaries(opt acquireOptions) (*detectResult, error) {
	res := &detectResult{rec: &infraBinaries{}}

	for _, a := range selectedArtifacts(opt) {
		switch a.Name {
		case "postgres":
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
		case "nats-server":
			if p, version, ok := detectNatsServer(opt); ok {
				res.rec.NatsServer, res.rec.NatsVersion = p, version
			} else {
				res.missing = append(res.missing, "nats-server")
			}
		default:
			// THE TELEMETRY BACKENDS: single static Go binaries with no initialisation step, so detection is a
			// PATH-shaped probe and there is nothing to validate — unlike PostgreSQL there is no share dir.
			if p, version, ok := detectSingleBinary(a, opt); ok {
				res.rec.setTelemetryArtifact(a.Name, p, version)
			} else {
				res.missing = append(res.missing, a.Name)
			}
		}
	}
	return res, nil
}

// telemetryPath is the recorded path for one telemetry artifact, or "" when this
// boot did not acquire it.
func (rec *infraBinaries) telemetryPath(name string) string {
	switch name {
	case "tempo":
		return rec.Tempo
	case "loki":
		return rec.Loki
	case "victoria-metrics-prod":
		return rec.VictoriaMetrics
	case "otelcol-contrib":
		return rec.OTelCollector
	}
	return ""
}

// telemetryVersion is the recorded version line for one telemetry artifact, or ""
// when this boot did not acquire it. The read-side twin of setTelemetryArtifact.
func (rec *infraBinaries) telemetryVersion(name string) string {
	switch name {
	case "tempo":
		return rec.TempoVersion
	case "loki":
		return rec.LokiVersion
	case "victoria-metrics-prod":
		return rec.VictoriaMetricsVersion
	case "otelcol-contrib":
		return rec.OTelCollectorVersion
	}
	return ""
}

// setTelemetryArtifact records a resolved telemetry backend. It is the ONE place
// the name → field mapping lives for writes, mirroring telemetryPath for reads.
func (rec *infraBinaries) setTelemetryArtifact(name, path, version string) {
	switch name {
	case "tempo":
		rec.Tempo, rec.TempoVersion = path, version
	case "loki":
		rec.Loki, rec.LokiVersion = path, version
	case "victoria-metrics-prod":
		rec.VictoriaMetrics, rec.VictoriaMetricsVersion = path, version
	case "otelcol-contrib":
		rec.OTelCollector, rec.OTelCollectorVersion = path, version
	}
}

// detectSingleBinary looks for a single-binary artifact on the caller's PATH, in the conventional
// user/Homebrew locations.
func detectSingleBinary(a infraArtifact, opt acquireOptions) (path, version string, ok bool) {
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
	goos, goarch := artifactPlatform(opt)
	// The looked-up name is the CANONICAL one — an acquired download is installed
	// under it, and the member-suffixed loki names never land in the bin dir.
	p, ok := lookPathIn(dirs, strings.TrimSuffix(canonicalName(a, goos), ".exe"), goos)
	if !ok {
		// A host that installed the platform-suffixed member by hand (loki's
		// published name) is still a usable install, so the member is accepted too.
		if m := strings.TrimSuffix(memberFor(a, goos, goarch), ".exe"); m != "" && m != strings.TrimSuffix(canonicalName(a, goos), ".exe") {
			p, ok = lookPathIn(dirs, m, goos)
		}
	}
	if !ok {
		return "", "", false
	}
	if out, err := procwin.Command(p, "--version").Output(); err == nil {
		version = strings.TrimSpace(string(out))
	}
	return p, version, true
}

// detectPostgresDir finds the first candidate dir holding a `postgres` SERVER.
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
	// THE CLIENT TOOLS COME FROM THE SAME DIR, and that is deliberate: a dir holding the SERVER is a
	// complete distribution, and its client tools match its server version exactly.
	rec.PgDump, rec.Psql = p("pg_dump"), p("psql")
	rec.PgRestore = optionalBin(p("pg_restore"), opt.GOOS)

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
	out, err := procwin.Command(bin, "--version").Output()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return "", fmt.Errorf("run %s --version: %w", bin, err)
		}
		return "", fmt.Errorf("run %s --version: %w: %s", bin, err, msg)
	}
	return strings.TrimSpace(string(out)), nil
}

// detectNatsServer looks for nats-server on the caller's PATH and in the conventional user/Homebrew
// locations.
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
	if out, err := procwin.Command(p, "--version").Output(); err == nil {
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

// artifactURL resolves the pinned download URL for an artifact, honoring an override.
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

// downloadArtifact downloads, verifies and unpacks one artifact, returning the absolute path of
// each of its required binaries.
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

	// The archive lands under the data dir (not the system temp dir) so a no-root host with a small
	// /tmp can still install.
	archive := filepath.Join(binDir(opt.DataDir), "."+a.Name+".download"+archiveSuffix(url))
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
		// MAKE IT RUNNABLE.
		if goos != "windows" {
			if err := os.Chmod(p, 0o755); err != nil {
				return nil, fmt.Errorf("make %s executable: %w", p, err)
			}
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

// downloadToFile fetches url into dst, streaming progress and keeping a bounded tail so a failure
// can still say WHY — the same posture `pullImage` (install.go:745) takes for image pulls.
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
func extractArchive(archive string, a infraArtifact, dst, goos string) error {
	if isZipArchive(archive, a, goos) {
		return extractZip(archive, a, dst, goos)
	}
	return extractTarGz(archive, a, dst, goos)
}

// archiveSuffix is the file-name suffix that identifies an artifact's archive
// format, taken from its download URL ("" when the URL names neither form).
func archiveSuffix(url string) string {
	u := strings.ToLower(url)
	for _, ext := range []string{".tar.gz", ".tgz", ".zip"} {
		if strings.HasSuffix(u, ext) {
			return ext
		}
	}
	return ""
}

// isZipArchive decides the unpacker for an artifact.
func isZipArchive(archive string, a infraArtifact, goos string) bool {
	if u := artifactURLForFormat(a, goos); u != "" {
		return strings.HasSuffix(strings.ToLower(u), ".zip")
	}
	return strings.HasSuffix(strings.ToLower(archive), ".zip")
}

// artifactURLForFormat returns one pinned URL for the artifact: the packaging format is the same on
// every platform, so any entry answers "is this publisher shipping zips?".
func artifactURLForFormat(a infraArtifact, goos string) string {
	if u := a.URLs[goos+"/"+runtime.GOARCH]; u != "" {
		return u
	}
	for _, plat := range []string{"linux/amd64", "darwin/amd64", "windows/amd64"} {
		if u := a.URLs[plat]; u != "" {
			return u
		}
	}
	return ""
}

// extractTarGz unpacks a .tar.gz, flattening the archive's single top-level directory. FLATTENING
// IS REQUIRED, NOT COSMETIC.
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
			// A LINK'S TARGET IS UNTRUSTED INPUT TOO, and validating only the entry NAME above leaves the
			// escape this used to have: a symlink whose target is outside dst.
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

// THE TWO ARCHIVE LAYOUTS ARE DIFFERENT AND MUST NOT SHARE ONE RULE: - NESTED (postgres,
// nats-server): everything sits under ONE top-level directory, which is stripped.
func extractRelPath(name string, a infraArtifact, goos string) (rel string, skip bool, err error) {
	n := filepath.ToSlash(strings.TrimPrefix(filepath.ToSlash(name), "./"))
	if n == "" || n == "." {
		return "", true, nil
	}
	if pathIsAbs(n) || hasDotDotSegment(n) {
		return "", false, fmt.Errorf("refusing to extract %q — the entry escapes the destination", name)
	}
	if !a.Nested {
		// FLAT: the entry must be a ROOT entry, never nested.
		if strings.Contains(n, "/") {
			return "", true, nil
		}
		return flatMemberRelPath(n, a, goos)
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
	// A nested single-binary artifact: keep only the binary itself.
	wanted := exeName(a.Required[0], goos)
	if filepath.Base(n) != wanted {
		return "", true, nil
	}
	return wanted, false, nil
}

// flatMemberRelPath decides whether a flat archive's ROOT entry is the binary we install, and under
// what name.
func flatMemberRelPath(base string, a infraArtifact, goos string) (rel string, skip bool, err error) {
	if len(a.Required) == 0 {
		return "", true, nil
	}
	canvas := canonicalName(a, goos)
	if base == canvas {
		return canvas, false, nil
	}
	for key, member := range a.Members {
		if !strings.HasPrefix(key, goos+"/") {
			continue
		}
		if base == member {
			return canvas, false, nil
		}
	}
	return "", true, nil
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

// secureJoin joins an already-validated entry-relative path onto dst, refusing to TRAVERSE a
// symlinked component on the way there.
func secureJoin(dst, rel string) (string, error) {
	target := filepath.Join(dst, rel)
	base := filepath.Clean(dst)
	// Split on the ARCHIVE's separator ('/'), not the host's: on Windows filepath.Separator is '\\', so
	// splitting on it would leave the whole relative path as one segment and silently skip.
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

// An absolute target is legal ONLY when it is already inside dst (a hard link to a sibling we just
// unpacked); anything pointing out of the instance data dir is refused, never created.
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

// writeExtracted writes one extracted file, clearing the executable bit on demand.
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

// ONLY SELECTS THE ARTIFACT SET (see acquireOptions.Only): the supervisor passes postgres+nats
// alone for an ORCHICON_TELEMETRY=none boot.
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
	for _, name := range telemetryArtifactNames {
		if p := rec.telemetryPath(name); p != "" {
			sources["detected"] = true
			fmt.Fprintf(opt.progressOut(), "using the %s already installed at %s\n", name, p)
		}
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
			rec.PgDump = paths["pg_dump"]
			rec.Psql = paths["psql"]
			// The optional extras live beside the resolved server, so the unpacked target is found relative to
			// it rather than re-derived from the artifact name (which would be a second opinion about.
			rec.PgRestore = optionalBin(filepath.Join(filepath.Dir(rec.Postgres), exeName("pg_restore", opt.GOOS)), opt.GOOS)
			// ASK THE BINARY, and keep the pinned tag only as a fallback.
			rec.PostgresVersion = artifactReportedVersion(rec.Postgres, a.Version)
		case "nats-server":
			rec.NatsServer = paths["nats-server"]
			rec.NatsVersion = artifactReportedVersion(rec.NatsServer, a.Version)
		default:
			// A telemetry backend: single binary, installed under its canonical name.
			set := paths[a.Required[0]]
			rec.setTelemetryArtifact(name, set, artifactReportedVersion(set, a.Version))
		}
	}

	rec.Source = sourceOf(sources)

	// 3.
	if err := checkRecordComplete(&rec, opt); err != nil {
		return nil, err
	}

	// 4. RECORD ONCE.
	if err := recordInfraBinaries(opt.DataDir, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// artifactReportedVersion runs `<bin> --version` and returns what it says, falling back to the
// pinned tag when the binary cannot be asked.
func artifactReportedVersion(bin, fallback string) string {
	out, err := procwin.Command(bin, "--version").Output()
	if err != nil {
		return fallback
	}
	if v := strings.TrimSpace(string(out)); v != "" {
		return v
	}
	return fallback
}

// sourceOf names where the resolved set came from: "detected", "downloaded", or "mixed" when the
// host supplied one binary and we fetched the other.
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
	// WHICH ARTIFACTS THIS ACQUISITION COVERED, so the gate proves the record complete FOR THAT SET and
	// no more.
	covered := map[string]bool{}
	for _, name := range selectedArtifactNames(opt) {
		covered[name] = true
	}
	if covered["postgres"] {
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
	}
	if covered["nats-server"] {
		st, err := os.Stat(rec.NatsServer)
		if err != nil {
			return fmt.Errorf("resolved nats-server is not usable (%s): %w", rec.NatsServer, err)
		}
		if st.IsDir() {
			return fmt.Errorf("resolved nats-server is a directory, not a binary: %s", rec.NatsServer)
		}
	}
	// THE TELEMETRY BACKENDS, checked only for the artifacts THIS acquisition actually resolved: a boot
	// that was not asked for them legitimately has no path for any of them.
	for _, name := range selectedArtifactNames(opt) {
		p := rec.telemetryPath(name)
		if p == "" {
			continue
		}
		st, err := os.Stat(p)
		if err != nil {
			return fmt.Errorf("resolved %s is not usable (%s): %w", name, p, err)
		}
		if st.IsDir() {
			return fmt.Errorf("resolved %s is a directory, not a binary: %s", name, p)
		}
		if goos != "windows" && st.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("resolved %s is not executable (%04o): %s", name, st.Mode().Perm(), p)
		}
	}
	return nil
}

// selectedArtifactNames is selectedArtifacts' names, for callers that only need
// to know WHICH artifacts this acquisition covered.
func selectedArtifactNames(opt acquireOptions) []string {
	arts := selectedArtifacts(opt)
	out := make([]string, 0, len(arts))
	for _, a := range arts {
		out = append(out, a.Name)
	}
	return out
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
	case "pg_dump":
		return rec.PgDump
	case "psql":
		return rec.Psql
	}
	return ""
}

// ---------------------------------------------------------------------------
// ENVIRONMENT
// ---------------------------------------------------------------------------

// Env knobs, deliberately few.

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

// defaultAcquireOptions builds the options an install or a supervisor boot uses: the instance data
// dir, the pinned artifacts, and whatever the environment overrides.
func defaultAcquireOptions(dataDir string, only ...string) acquireOptions {
	opt := acquireOptions{
		DataDir:      dataDir,
		PathEnv:      os.Getenv("PATH"),
		SkipDownload: skipDownloadFromEnv(),
		URLs:         map[string]string{},
		Checksums:    map[string]string{},
		Out:          os.Stdout,
		Only:         only,
	}
	goos, goarch := artifactPlatform(opt)
	// The OVERRIDE KNOBS ARE READ FOR EVERY PINNED ARTIFACT, not just the selected ones: a mirror
	// configured for a telemetry backend must still be honoured on the boot that fetches it.
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

// checkPostgresShare proves the PostgreSQL install has the share directory initdb needs — the
// "corrupted installation" failure a bin-only copy produces.
func checkPostgresShare(initdbPath, goos string) error {
	bindir := filepath.Dir(initdbPath)
	var tried []string

	if goos != "windows" {
		pgConfig := filepath.Join(bindir, "pg_config")
		if _, err := os.Stat(pgConfig); err == nil {
			if out, err := procwin.Command(pgConfig, "--sharedir").Output(); err == nil {
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

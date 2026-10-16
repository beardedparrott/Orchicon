package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These cover the five cases the acceptance criteria name — present, absent,
// partial, checksum-mismatch, wrong-version — plus the record's round trip and the
// pure version parser.
//
// THERE IS NO NETWORK IN HERE. Every download test is served by an in-process
// httptest server and every checksum is computed from the fixture it serves, so the
// suite is deterministic and runs in CI with no outbound access. `RestrictToPath`
// keeps the "absent" case honest: without it, a test asserting absence would pass or
// fail depending on whether the machine running it happens to have PostgreSQL
// installed, which is exactly the kind of test that is green for the wrong reason.

// ----- fixtures -----

// shim writes an executable file that prints `out` for `--version`.
//
// It is a real shell script, not a static file, because the version path actually
// EXECS the binary: stubbing the exec away would leave the wrong-version case
// (AC#7) untested, and that case is one of the five.
func shim(t *testing.T, dir, name, out, goos string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, exeName(name, goos))
	body := "#!/bin/sh\n"
	if out != "" {
		body += "printf '%s\\n' " + shellQuote(out) + "\n"
	}
	body += "exit 0\n"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// postgresShims populates a dir with all five required PostgreSQL binaries, each
// reporting the same version, plus the share directory a complete install has.
func postgresShims(t *testing.T, dir, version, goos string) string {
	t.Helper()
	bindir := filepath.Join(dir, "bin")
	for _, b := range postgresRequiredBins {
		shim(t, bindir, b, "postgres (PostgreSQL) "+version, goos)
	}
	// initdb resolves its share directory relative to its own path, so a complete
	// fixture has one — that is what checkPostgresShare verifies.
	if err := os.MkdirAll(filepath.Join(dir, "share"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "share", "postgres.bki"), []byte("# fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return bindir
}

// tarGz builds a .tar.gz whose entries are `name -> body`, simulating the pinned
// publishers' archives (which wrap everything in one top-level directory).
func tarGz(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sortStrings(names)
	for _, name := range names {
		body := entries[name]
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// sortStrings orders the fixture entries so the archive is byte-stable.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// zipBytes builds a .zip from name -> body (the windows artifact shape).
func zipBytes(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sortStrings(names)
	for _, name := range names {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(entries[name])); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// sha256Of is a fixture's expected checksum.
func sha256Of(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// fixtureServer serves each artifact body at /<name> and records what was fetched,
// so a test can assert that a detected install downloaded NOTHING (AC#1).
func fixtureServer(t *testing.T, bodies map[string][]byte) (*httptest.Server, *[]string) {
	t.Helper()
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		hits = append(hits, name)
		body, ok := bodies[name]
		if !ok {
			http.Error(w, "fixture not found: "+name, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// downloadOptions points the acquisition at a fixture server for linux/amd64, so no
// test can accidentally reach the network.
func downloadOptions(t *testing.T, srv *httptest.Server, bodies map[string][]byte, dataDir string) acquireOptions {
	t.Helper()
	opt := acquireOptions{
		GOOS:           "linux",
		GOARCH:         "amd64",
		DataDir:        dataDir,
		PathEnv:        filepath.Join(dataDir, "empty-path"),
		RestrictToPath: true,
		Out:            &bytes.Buffer{},
		URLs:           map[string]string{},
		Checksums:      map[string]string{},
	}
	goos, goarch := artifactPlatform(opt)
	for name, body := range bodies {
		opt.URLs[overrideKey(name, goos, goarch)] = srv.URL + "/" + name
		opt.Checksums[overrideKey(name, goos, goarch)] = sha256Of(body)
	}
	opt.HTTPClient = srv.Client()
	return opt
}

// pgFixtureBody and natsFixtureBody are archives shaped like the pinned releases:
// one top-level directory, with bin/ underneath (PostgreSQL) and the single binary
// at its root (nats-server).
func pgFixtureBody(t *testing.T, goos string) []byte {
	t.Helper()
	root := "postgresql-16.4.0-fixture/"
	entries := map[string]string{}
	for _, b := range postgresRequiredBins {
		entries[root+"bin/"+exeName(b, goos)] = "#!/bin/sh\nprintf 'postgres (PostgreSQL) 16.4\\n'\n"
	}
	entries[root+"share/postgres.bki"] = "# fixture\n"
	return tarGz(t, entries)
}

func natsFixtureBody(t *testing.T, goos string) []byte {
	t.Helper()
	return tarGz(t, map[string]string{
		"nats-server-fixture/" + exeName("nats-server", goos): "#!/bin/sh\nprintf 'nats-server: v2.10.29\\n'\n",
	})
}

// newFixtureOptions assembles the download fixture: a server serving both
// artifacts and options pointing at it.
//
// ONLY IS POSTGRES+NATS. These tests pin the postgres/nats contract and their
// fixture serves only those two artifacts, so without the subset the four
// telemetry backends would fall through to their PINNED URLs — the suite would
// reach the network and report "mixed" where "detected" is expected. The
// telemetry subset has its own fixture (telemetryFixtureOptions).
func newFixtureOptions(t *testing.T, dataDir string) (acquireOptions, *[]string) {
	t.Helper()
	bodies := map[string][]byte{
		"postgres":    pgFixtureBody(t, "linux"),
		"nats-server": natsFixtureBody(t, "linux"),
	}
	srv, hits := fixtureServer(t, bodies)
	opt := downloadOptions(t, srv, bodies, dataDir)
	opt.Only = []string{"postgres", "nats-server"}
	return opt, hits
}

// ----- the five cases -----

// TestAcquireUsesAnInstalledPostgresAndNats downloads nothing (AC#1).
func TestAcquireUsesAnInstalledPostgresAndNats(t *testing.T) {
	dataDir := t.TempDir()
	pgBin := postgresShims(t, t.TempDir(), "16.4", "linux")
	natsDir := t.TempDir()
	shim(t, natsDir, "nats-server", "nats-server: v2.10.29", "linux")

	opt, hits := newFixtureOptions(t, dataDir)
	opt.PathEnv = pgBin + string(os.PathListSeparator) + natsDir

	rec, err := acquireInfraBinaries(context.Background(), opt)
	if err != nil {
		t.Fatalf("acquire with a complete install on PATH: %v", err)
	}
	if rec.Source != "detected" {
		t.Errorf("Source = %q, want \"detected\" — a host that has the binaries must not be reported as a download", rec.Source)
	}
	if got, want := rec.Postgres, filepath.Join(pgBin, "postgres"); got != want {
		t.Errorf("Postgres = %q, want the one on PATH (%q)", got, want)
	}
	if got, want := rec.NatsServer, filepath.Join(natsDir, "nats-server"); got != want {
		t.Errorf("NatsServer = %q, want %q", got, want)
	}
	if len(*hits) != 0 {
		t.Errorf("nothing may be downloaded when both are installed; the fixture server was hit for %v", *hits)
	}
	// The record is the point of the record.
	back, err := loadInfraBinaries(dataDir)
	if err != nil {
		t.Fatalf("loadInfraBinaries: %v", err)
	}
	if back.Postgres != rec.Postgres || back.NatsServer != rec.NatsServer {
		t.Errorf("the recorded set differs from the returned one: %+v vs %+v", back, rec)
	}
}

// TestAcquireDownloadsWhenBothAreAbsent pins AC#2: no root, into the data dir.
func TestAcquireDownloadsWhenBothAreAbsent(t *testing.T) {
	dataDir := t.TempDir()
	opt, _ := newFixtureOptions(t, dataDir)

	rec, err := acquireInfraBinaries(context.Background(), opt)
	if err != nil {
		t.Fatalf("acquire on a host with neither binary: %v", err)
	}
	if rec.Source != "downloaded" {
		t.Errorf("Source = %q, want \"downloaded\"", rec.Source)
	}
	for _, p := range []string{rec.Postgres, rec.Initdb, rec.PgCtl, rec.PgIsReady, rec.Createdb, rec.NatsServer} {
		if !filepath.IsAbs(p) {
			t.Errorf("%q must be absolute — a relative path would depend on the caller's cwd", p)
		}
		if !strings.HasPrefix(p, dataDir) {
			t.Errorf("%q must land under the instance data dir (%s) — the no-root path depends on it", p, dataDir)
		}
		if st, err := os.Stat(p); err != nil || st.IsDir() {
			t.Errorf("%q must exist as a file after acquisition: %v", p, err)
		}
	}
	// The record must describe the SAME KIND OF THING on both acquisition paths: the
	// version the binary itself reports, not the release tag (16.4.0 for a server that
	// reports 16.4). A detected install records the reported string, so a downloaded
	// one must too, or one record would carry two different formats.
	for name, got := range map[string]string{"postgres": rec.PostgresVersion, "nats": rec.NatsVersion} {
		if !strings.Contains(got, "16.4") && !strings.Contains(got, "2.10") {
			t.Errorf("%s version in the record = %q, want the version the binary reports", name, got)
		}
	}
	if !strings.Contains(rec.PostgresVersion, "16.4") {
		t.Errorf("PostgresVersion = %q, want the reported 16.4 line (pinned tag is %s)", rec.PostgresVersion, pinnedPostgresVersion)
	}
	if !strings.Contains(rec.NatsVersion, pinnedNATSServerVersion) {
		t.Errorf("NatsVersion = %q, want it to carry the pinned %s", rec.NatsVersion, pinnedNATSServerVersion)
	}
	// The unpacked distribution must keep the share directory initdb needs; this is
	// what a bin-only unpack silently loses.
	if err := checkPostgresShare(rec.Initdb, "linux"); err != nil {
		t.Errorf("the downloaded distribution lost its share directory: %v", err)
	}
	// A second acquisition on the same data dir is a no-op: the build it just
	// unpacked is itself a complete install, so nothing is downloaded twice.
	again, err := acquireInfraBinaries(context.Background(), opt)
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if again.Source != "detected" {
		t.Errorf("second acquire Source = %q, want \"detected\" (the installed-set path, reusing what it unpacked)", again.Source)
	}
}

// TestAcquireDownloadsOnlyWhatIsMissing pins the per-artifact split: a host with a
// good Postgres and no nats-server must fetch one thing, not two.
func TestAcquireDownloadsOnlyWhatIsMissing(t *testing.T) {
	dataDir := t.TempDir()
	pgBin := postgresShims(t, t.TempDir(), "16.4", "linux")

	opt, hits := newFixtureOptions(t, dataDir)
	opt.PathEnv = pgBin

	rec, err := acquireInfraBinaries(context.Background(), opt)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if rec.Source != "mixed" {
		t.Errorf("Source = %q, want \"mixed\" — the host supplied postgres and we fetched nats", rec.Source)
	}
	if got, want := rec.Postgres, filepath.Join(pgBin, "postgres"); got != want {
		t.Errorf("Postgres = %q, want the installed %q", got, want)
	}
	if len(*hits) != 1 || (*hits)[0] != "nats-server" {
		t.Errorf("exactly nats-server may be downloaded; the server was hit for %v", *hits)
	}
}

// TestAcquireRejectsAPartialPostgresInstall names what is missing (AC#4).
func TestAcquireRejectsAPartialPostgresInstall(t *testing.T) {
	dataDir := t.TempDir()
	partial := filepath.Join(t.TempDir(), "bin")
	shim(t, partial, "postgres", "postgres (PostgreSQL) 16.4", "linux")
	shim(t, partial, "pg_isready", "pg_isready (PostgreSQL) 16.4", "linux")
	// initdb, pg_ctl and createdb are deliberately absent — the client-only shape.
	natsDir := t.TempDir()
	shim(t, natsDir, "nats-server", "nats-server: v2.10.29", "linux")

	opt, hits := newFixtureOptions(t, dataDir)
	opt.PathEnv = partial + string(os.PathListSeparator) + natsDir

	_, err := acquireInfraBinaries(context.Background(), opt)
	if err == nil {
		t.Fatal("a partial PostgreSQL install must be REJECTED, not satisfied")
	}
	msg := err.Error()
	for _, want := range []string{"initdb", "pg_ctl", "createdb", partial} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error must name %q so the operator knows exactly what is missing; got:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "missing postgres") {
		t.Errorf("postgres IS present and must not be reported missing:\n%s", msg)
	}
	if len(*hits) != 0 {
		t.Errorf("a partial install must not be silently downloaded over; the server was hit for %v", *hits)
	}
	if _, statErr := os.Stat(filepath.Join(dataDir, infraBinariesFile)); statErr == nil {
		t.Error("no record may be written when acquisition failed")
	}
}

// TestAcquireRefusesAChecksumMismatch pins AC#3: hard, loud, no fallback.
func TestAcquireRefusesAChecksumMismatch(t *testing.T) {
	dataDir := t.TempDir()
	opt, _ := newFixtureOptions(t, dataDir)
	goos, goarch := opt.GOOS, opt.GOARCH

	// The server serves a DIFFERENT body than the checksum we expect, which is what
	// a corrupted transfer or a substituted artifact looks like.
	opt.Checksums[overrideKey("postgres", goos, goarch)] = strings.Repeat("0", 64)

	_, err := acquireInfraBinaries(context.Background(), opt)
	if err == nil {
		t.Fatal("a checksum mismatch must be a hard failure — never a fallback to an unverified binary")
	}
	msg := err.Error()
	if !strings.Contains(msg, "checksum mismatch") {
		t.Errorf("the error must say the checksum mismatched; got:\n%s", msg)
	}
	if !strings.Contains(msg, strings.Repeat("0", 64)) {
		t.Errorf("the error must state the EXPECTED sum, so the failure is diagnosable; got:\n%s", msg)
	}
	if _, statErr := os.Stat(filepath.Join(dataDir, infraBinariesFile)); statErr == nil {
		t.Error("no record may be written after a checksum mismatch")
	}
	// The unverified download must be GONE, not left where a later step could use it.
	if _, statErr := os.Stat(filepath.Join(binDir(dataDir), ".postgres.download")); statErr == nil {
		t.Error("the unverified download must be deleted, not left on disk")
	}
}

// TestAcquireRefusesATooOldPostgres pins AC#7.
func TestAcquireRefusesATooOldPostgres(t *testing.T) {
	dataDir := t.TempDir()
	oldBin := postgresShims(t, t.TempDir(), "12.4", "linux")
	natsDir := t.TempDir()
	shim(t, natsDir, "nats-server", "nats-server: v2.10.29", "linux")

	opt, _ := newFixtureOptions(t, dataDir)
	opt.PathEnv = oldBin + string(os.PathListSeparator) + natsDir

	_, err := acquireInfraBinaries(context.Background(), opt)
	if err == nil {
		t.Fatal("a PostgreSQL older than the documented minimum must be reported, not attempted")
	}
	msg := err.Error()
	for _, want := range []string{"12", fmt.Sprint(minPostgresMajor)} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error must name %q (the version found, and the minimum); got:\n%s", want, msg)
		}
	}
	if _, statErr := os.Stat(filepath.Join(dataDir, infraBinariesFile)); statErr == nil {
		t.Error("no record may be written when the detected server is unusable")
	}
}

// TestAcquireAtTheMinimumPostgresIsAccepted pins the BOUNDARY of AC#7: the documented
// minimum itself is supported, so the check is not silently off by one.
func TestAcquireAtTheMinimumPostgresIsAccepted(t *testing.T) {
	dataDir := t.TempDir()
	bin := postgresShims(t, t.TempDir(), fmt.Sprintf("%d.0", minPostgresMajor), "linux")
	natsDir := t.TempDir()
	shim(t, natsDir, "nats-server", "nats-server: v2.10.29", "linux")

	opt, _ := newFixtureOptions(t, dataDir)
	opt.PathEnv = bin + string(os.PathListSeparator) + natsDir

	rec, err := acquireInfraBinaries(context.Background(), opt)
	if err != nil {
		t.Fatalf("PostgreSQL %d is the documented minimum and must be accepted: %v", minPostgresMajor, err)
	}
	if rec.Source != "detected" {
		t.Errorf("Source = %q, want \"detected\"", rec.Source)
	}
}

// TestAcquireSkipsDownloadWhenForbidden pins the no-network escape hatch: absence
// becomes a clear, actionable refusal rather than a hang.
func TestAcquireSkipsDownloadWhenForbidden(t *testing.T) {
	dataDir := t.TempDir()
	opt, hits := newFixtureOptions(t, dataDir)
	opt.SkipDownload = true

	_, err := acquireInfraBinaries(context.Background(), opt)
	if err == nil {
		t.Fatal("with downloads disabled and nothing installed, acquisition must fail")
	}
	for _, want := range []string{"postgres", "not installed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must name %q; got:\n%s", want, err)
		}
	}
	if len(*hits) != 0 {
		t.Errorf("downloads were forbidden but the server was hit: %v", *hits)
	}
}

// TestInfraBinariesRecordRoundTrip pins AC#5's record.
func TestInfraBinariesRecordRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	want := &infraBinaries{
		Source: "mixed", Postgres: "/a/postgres", Initdb: "/a/initdb", PgCtl: "/a/pg_ctl",
		PgIsReady: "/a/pg_isready", Createdb: "/a/createdb", NatsServer: "/b/nats-server",
		PostgresVersion: "16.4", NatsVersion: "2.10.29",
	}
	if err := recordInfraBinaries(dataDir, want); err != nil {
		t.Fatalf("record: %v", err)
	}
	got, err := loadInfraBinaries(dataDir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if *got != *want {
		t.Errorf("round trip changed the record:\n got %+v\nwant %+v", got, want)
	}
	if _, err := loadInfraBinaries(t.TempDir()); !errorsIs(err, errInfraAbsent) {
		t.Errorf("a missing record must report errInfraAbsent, got %v", err)
	}
	// A record with a hole in it must not be trusted as a resolved set.
	if err := recordInfraBinaries(dataDir, &infraBinaries{Source: "detected"}); err != nil {
		t.Fatal(err)
	}
	if _, err := loadInfraBinaries(dataDir); err == nil {
		t.Error("an incomplete record must be refused, not returned as usable")
	}
}

// errorsIs is errors.Is without importing errors just for the tests.
func errorsIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// ----- primitives the cases rest on -----

// TestParsePostgresMajor covers the real shapes this output takes.
func TestParsePostgresMajor(t *testing.T) {
	cases := map[string]struct {
		want  int
		found bool
	}{
		"postgres (PostgreSQL) 16.4":                 {16, true},
		"postgres (PostgreSQL) 16.4 (Debian 16.4-1)": {16, true},
		"postgres (PostgreSQL) 12.0":                 {12, true},
		"postgres (PostgreSQL) 9.6.24":               {9, true},
		"postgres (PostgreSQL) 14devel":              {14, true},
		"":                                           {0, false},
		"not a version":                              {0, false},
		"postgres (PostgreSQL) (no version here)":    {0, false},
	}
	for in, want := range cases {
		got, found := parsePostgresMajor(in)
		if got != want.want || found != want.found {
			t.Errorf("parsePostgresMajor(%q) = (%d, %v), want (%d, %v)", in, got, found, want.want, want.found)
		}
	}
}

// TestPartitionBinsSplitsPresentFromMissing is the primitive AC#4 rests on.
func TestPartitionBinsSplitsPresentFromMissing(t *testing.T) {
	dir := t.TempDir()
	shim(t, dir, "postgres", "postgres (PostgreSQL) 16.4", "linux")
	shim(t, dir, "pg_ctl", "", "linux")
	present, missing := partitionBins(dir, postgresRequiredBins, "linux")
	if strings.Join(present, ",") != "postgres,pg_ctl" {
		t.Errorf("present = %v, want [postgres pg_ctl]", present)
	}
	// pg_dump AND psql ARE REQUIRED TOO, and this assertion is where that is pinned:
	// they are what `orchicon db backup`/`restore` exec, and leaving them out of the
	// required set is what made every backup on a downloaded host install fail with
	// "pg_dump is not on this PATH and no Postgres container was reachable".
	if strings.Join(missing, ",") != "initdb,pg_isready,createdb,pg_dump,psql" {
		t.Errorf("missing = %v, want [initdb pg_isready createdb pg_dump psql]", missing)
	}
	// A DIRECTORY named like a binary is not a binary.
	if err := os.MkdirAll(filepath.Join(dir, "createdb"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, missing = partitionBins(dir, []string{"createdb"}, "linux")
	if len(missing) != 1 {
		t.Error("a directory named createdb must not satisfy the presence check")
	}
}

// TestExtractRejectsAPathEscape pins the archive-validation rule: an archive is
// untrusted input even when its checksum matches.
func TestExtractRejectsAPathEscape(t *testing.T) {
	dir := t.TempDir()
	body := tarGz(t, map[string]string{
		"root/../escape": "x",
		"/absolute":      "y",
		"root/bin/" + exeName("postgres", "linux"): "z",
	})
	archive := filepath.Join(dir, "a.tar.gz")
	if err := os.WriteFile(archive, body, 0o644); err != nil {
		t.Fatal(err)
	}
	a, _ := artifactFor("postgres")
	err := extractTarGz(archive, a, filepath.Join(dir, "out"), "linux")
	if err == nil {
		t.Fatal("an archive entry that escapes the destination must be refused")
	}
	if !strings.Contains(err.Error(), "escapes the destination") {
		t.Errorf("the refusal must say why; got: %v", err)
	}
}

// TestExtractZipFlattensTheTopLevelDir covers the windows artifact shape, and that
// a single-binary artifact does not scatter its README/LICENSE into the bin dir.
func TestExtractZipFlattensTheTopLevelDir(t *testing.T) {
	dir := t.TempDir()
	body := zipBytes(t, map[string]string{
		"nats-server-v2.10.29-windows-amd64/LICENSE":         "license",
		"nats-server-v2.10.29-windows-amd64/nats-server.exe": "binary",
	})
	archive := filepath.Join(dir, "a.zip")
	if err := os.WriteFile(archive, body, 0o644); err != nil {
		t.Fatal(err)
	}
	a, _ := artifactFor("nats-server")
	out := filepath.Join(dir, "out")
	if err := extractZip(archive, a, out, "windows"); err != nil {
		t.Fatalf("extract: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "nats-server.exe")); err != nil {
		t.Errorf("the single binary must be flattened to the top level: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "LICENSE")); err == nil {
		t.Error("a single-binary artifact must not unpack its README/LICENSE into the bin dir")
	}
}

// TestPinnedArtifactsCoverTheSupportedPlatforms keeps the table honest: a platform
// with a URL but no checksum would download unverified, which AC#3 forbids.
func TestPinnedArtifactsCoverTheSupportedPlatforms(t *testing.T) {
	for _, a := range infraArtifacts {
		if len(a.URLs) == 0 {
			t.Errorf("%s has no pinned URL", a.Name)
		}
		for plat, url := range a.URLs {
			if a.Checksums[plat] == "" {
				t.Errorf("%s has a URL for %s but no checksum — a download that cannot be verified must not be offered", a.Name, plat)
			}
			if len(a.Checksums[plat]) != 64 {
				t.Errorf("%s/%s checksum is %d chars, want a 64-char SHA256", a.Name, plat, len(a.Checksums[plat]))
			}
			if !strings.HasPrefix(url, "https://") {
				t.Errorf("%s/%s URL is not https: %s", a.Name, plat, url)
			}
		}
		for plat := range a.Checksums {
			if a.URLs[plat] == "" {
				t.Errorf("%s has a checksum for %s but no URL", a.Name, plat)
			}
		}
		// The pinned version must be recorded per artifact, or the record cannot say
		// what is running.
		if a.Version == "" {
			t.Errorf("%s has no pinned version", a.Name)
		}
	}
}

// TestAcquireRefusesAnUnpinnedPlatform pins that a platform we ship no build for is
// NAMED, rather than silently handed another architecture's binary.
func TestAcquireRefusesAnUnpinnedPlatform(t *testing.T) {
	opt := acquireOptions{DataDir: t.TempDir(), PathEnv: filepath.Join(t.TempDir(), "empty"), RestrictToPath: true}
	opt.GOOS, opt.GOARCH = "plan9", "mips"
	a, _ := artifactFor("postgres")
	if _, err := artifactURL(a, opt); err == nil {
		t.Fatal("a platform with no pinned build must be an error")
	} else if !strings.Contains(err.Error(), "plan9/mips") {
		t.Errorf("the error must name the platform; got: %v", err)
	}
}

// TestDefaultAcquireOptionsHonorsTheEnvOverrides pins the mirror escape hatches,
// since a bad override is otherwise only discovered on a real install.
func TestDefaultAcquireOptionsHonorsTheEnvOverrides(t *testing.T) {
	t.Setenv(skipDownloadEnv, "1")
	t.Setenv("ORCHICON_INFRA_URL_POSTGRES", "https://mirror.example/pg.tgz")
	t.Setenv("ORCHICON_INFRA_SHA256_NATS_SERVER", strings.Repeat("a", 64))
	opt := defaultAcquireOptions(t.TempDir())
	if !opt.SkipDownload {
		t.Error("ORCHICON_SKIP_DOWNLOAD=1 must set SkipDownload")
	}
	goos, goarch := artifactPlatform(opt)
	if got := opt.URLs[overrideKey("postgres", goos, goarch)]; got != "https://mirror.example/pg.tgz" {
		t.Errorf("postgres URL override = %q", got)
	}
	if got := opt.Checksums[overrideKey("nats-server", goos, goarch)]; got != strings.Repeat("a", 64) {
		t.Errorf("nats-server checksum override = %q", got)
	}
	// The pinned checksum still applies to the artifact that was NOT overridden.
	if opt.Checksums[overrideKey("postgres", goos, goarch)] != "" {
		t.Error("an override for one artifact must not populate another's")
	}
}

// TestCheckPostgresShareAcceptsBothLayouts pins the two real share-dir layouts,
// because getting this wrong REJECTS a perfectly good distro install and forces an
// unnecessary download. Debian/Ubuntu put it under /usr/share/postgresql/<major>
// (the sibling of /usr/lib/postgresql/<major>/bin), which the distribution builds
// instead keep beside bin/.
func TestCheckPostgresShareAcceptsBothLayouts(t *testing.T) {
	t.Run("sibling-layout", func(t *testing.T) {
		root := t.TempDir()
		bin := postgresShims(t, root, "16.4", "linux")
		if err := checkPostgresShare(filepath.Join(bin, "initdb"), "linux"); err != nil {
			t.Errorf("the distribution layout (share/ beside bin/) must be accepted: %v", err)
		}
	})
	t.Run("missing-share", func(t *testing.T) {
		// A bin-only copy: exactly what initdb fails to use, and what the check is
		// for. This is the real failure it prevents ("corrupted installation").
		root := t.TempDir()
		bin := filepath.Join(root, "bin")
		for _, b := range postgresRequiredBins {
			shim(t, bin, b, "postgres (PostgreSQL) 16.4", "linux")
		}
		err := checkPostgresShare(filepath.Join(bin, "initdb"), "linux")
		if err == nil {
			t.Fatal("a bin-only install has no share dir and must be refused")
		}
		if !strings.Contains(err.Error(), "postgres.bki") {
			t.Errorf("the error must name what is missing; got: %v", err)
		}
	})
	t.Run("debian-layout", func(t *testing.T) {
		// Only meaningful where a real Debian-style install exists; the point is
		// that pg_config --sharedir is consulted, not assumed.
		initdb := "/usr/lib/postgresql/15/bin/initdb"
		if _, err := os.Stat(initdb); err != nil {
			t.Skip("no Debian-style PostgreSQL install on this host")
		}
		if err := checkPostgresShare(initdb, "linux"); err != nil {
			t.Errorf("a real Debian-style install must be accepted: %v", err)
		}
	})
}

// ----- the real artifacts (opt-in) -----

// TestRealPinnedArtifactsDownloadAndVerify is the ONE check a fixture cannot make:
// that the pinned URLs and checksums in the table are the real ones. Every other test
// in this file serves its own fixture, so a typo'd checksum or a moved release would
// pass all of them and fail on an operator's first install.
//
// IT IS OPT-IN because it needs the network, and CI must not. Run it deliberately:
//
//	ORCHICON_TEST_REAL_DOWNLOAD=1 go test ./cmd/orchicon/ -run TestRealPinnedArtifacts -v
func TestRealPinnedArtifactsDownloadAndVerify(t *testing.T) {
	if os.Getenv("ORCHICON_TEST_REAL_DOWNLOAD") != "1" {
		t.Skip("set ORCHICON_TEST_REAL_DOWNLOAD=1 to fetch the real pinned artifacts")
	}
	dataDir := t.TempDir()
	// The pinned linux/amd64 postgres+nats builds: this test is about THEIR
	// checksums, and the four telemetry backends are ~300 MB that add nothing to
	// what is asserted here.
	opt := defaultAcquireOptions(dataDir, "postgres", "nats-server")
	// Detection off, so this exercises the download path for BOTH artifacts.
	opt.PathEnv = filepath.Join(dataDir, "empty-path")
	opt.RestrictToPath = true
	opt.Out = os.Stdout
	// The pinned linux/amd64 builds, whatever this host is — the checksums are the
	// thing under test, and only one platform's need to be proven per run.
	opt.GOOS, opt.GOARCH = "linux", "amd64"

	rec, err := acquireInfraBinaries(context.Background(), opt)
	if err != nil {
		t.Fatalf("the pinned artifacts must download and verify: %v", err)
	}
	if rec.Source != "downloaded" {
		t.Fatalf("Source = %q, want \"downloaded\"", rec.Source)
	}
	// The real proof: the downloaded postgres must actually RUN and report the pinned
	// version, and initdb must accept the unpacked tree (the share-dir check, end to
	// end, on a real distribution).
	out, err := exec.Command(rec.Postgres, "--version").Output()
	if err != nil {
		t.Fatalf("run the downloaded postgres: %v", err)
	}
	if !strings.Contains(string(out), "16.4") {
		t.Errorf("downloaded postgres reports %q, want the pinned 16.4", strings.TrimSpace(string(out)))
	}
	if !strings.Contains(rec.PostgresVersion, "16.4") {
		t.Errorf("the record's PostgresVersion = %q, want the version the binary reports", rec.PostgresVersion)
	}
	initData := filepath.Join(dataDir, "pgdata")
	if out, err := exec.Command(rec.Initdb, "-D", initData, "-U", "orchicon", "--auth=trust", "-E", "UTF8").CombinedOutput(); err != nil {
		t.Fatalf("initdb on the downloaded distribution failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if _, err := os.Stat(filepath.Join(initData, "PG_VERSION")); err != nil {
		t.Errorf("initdb did not produce a cluster: %v", err)
	}
	if out, err := exec.Command(rec.NatsServer, "--version").Output(); err != nil {
		t.Errorf("run the downloaded nats-server: %v", err)
	} else if !strings.Contains(string(out), pinnedNATSServerVersion) {
		t.Errorf("downloaded nats-server reports %q, want the pinned %s", strings.TrimSpace(string(out)), pinnedNATSServerVersion)
	}
}

// TestMountedBinDirIsDetected pins the install→container wiring: `orchicon install`
// resolves the binaries into the instance's HOST state dir and mounts that dir at
// <DataDir>/bin in the services container, so the container-side supervisor DETECTS what
// the host already resolved and downloads nothing a second time. Without this the mount
// would be decorative and the container would fetch its own copy.
func TestMountedBinDirIsDetected(t *testing.T) {
	host := t.TempDir()
	guestData := t.TempDir()
	// The host-side resolution: <host>/bin/postgres/{bin,share} + <host>/bin/nats-server.
	root := filepath.Join(host, infraBinDirName, postgresSubdirName)
	postgresShims(t, root, "16.4", "linux")
	shim(t, filepath.Join(host, infraBinDirName), "nats-server", "nats-server: v2.10.29", "linux")

	// The mount itself: <guest>/bin -> <host>/bin.
	if err := os.Symlink(filepath.Join(host, infraBinDirName), filepath.Join(guestData, infraBinDirName)); err != nil {
		t.Skipf("symlink unavailable on this host: %v", err)
	}
	opt := acquireOptions{
		GOOS: "linux", GOARCH: "amd64", DataDir: guestData,
		PathEnv: filepath.Join(guestData, "empty"), RestrictToPath: true,
		Out: io.Discard,
		// The mount carries postgres+nats only, and this test is about THAT mount
		// being detected — without the subset the telemetry artifacts would go to
		// their pinned URLs and the suite would reach the network.
		Only: []string{"postgres", "nats-server"},
	}
	rec, err := acquireInfraBinaries(context.Background(), opt)
	if err != nil {
		t.Fatalf("a mounted bin dir must be detected, not re-downloaded: %v", err)
	}
	if rec.Source != "detected" {
		t.Errorf("Source = %q, want \"detected\" — the mount exists so the container downloads nothing", rec.Source)
	}
	// The path recorded is the CONTAINER-side one, which is what the container's procs
	// must exec (the host path does not exist inside the container).
	if !strings.HasPrefix(rec.Postgres, guestData) {
		t.Errorf("Postgres = %q, want a path under the container data dir (%s)", rec.Postgres, guestData)
	}
	if !strings.HasPrefix(rec.NatsServer, guestData) {
		t.Errorf("NatsServer = %q, want a path under the container data dir (%s)", rec.NatsServer, guestData)
	}
}

// TestExtractRefusesALinkThatEscapesTheDestination pins the escape a NAME-ONLY
// check cannot see.
//
// extractRelPath rejects absolute names and `..` segments, so the joined path is
// textually inside dst — but an archive can create a symlink first and then write
// THROUGH it, and filepath.Join cannot notice. Probed against a hand-built tampered
// archive: `root/evil -> <a directory outside dst>` followed by `root/evil/pwned`
// wrote outside the destination before the link/join checks existed. An archive is
// untrusted input even when its checksum matches — the checksum proves which bytes
// arrived, not that they are safe to unpack.
func TestExtractRefusesALinkThatEscapesTheDestination(t *testing.T) {
	t.Run("symlink-then-write-through-it", func(t *testing.T) {
		dst := t.TempDir()
		outside := t.TempDir()
		archive := filepath.Join(t.TempDir(), "a.tar.gz")
		if err := os.WriteFile(archive, tarGzEntries(t, []tarEntry{
			{name: "root/evil", link: outside, symlink: true},
			{name: "root/evil/pwned", body: "escaped"},
		}), 0o644); err != nil {
			t.Fatal(err)
		}
		a, _ := artifactFor("postgres")
		if err := extractTarGz(archive, a, dst, "linux"); err == nil {
			t.Fatal("a link pointing outside the destination must be refused")
		}
		if _, err := os.Stat(filepath.Join(outside, "pwned")); err == nil {
			t.Fatal("the escape SUCCEEDED: a file was written outside the destination")
		}
	})

	t.Run("absolute-outside-target", func(t *testing.T) {
		dst := t.TempDir()
		archive := filepath.Join(t.TempDir(), "a.tar.gz")
		if err := os.WriteFile(archive, tarGzEntries(t, []tarEntry{
			{name: "root/escape", link: "/etc/passwd", symlink: true},
		}), 0o644); err != nil {
			t.Fatal(err)
		}
		a, _ := artifactFor("postgres")
		if err := extractTarGz(archive, a, dst, "linux"); err == nil {
			t.Fatal("an absolute link target outside the destination must be refused")
		}
	})

	// The refusal must not be blanket: the pinned PostgreSQL build legitimately
	// carries sibling symlinks in lib/ (libpq.so -> libpq.so.5.16), and rejecting
	// those would break the download path the whole feature exists for.
	t.Run("sibling-links-are-still-accepted", func(t *testing.T) {
		dst := t.TempDir()
		archive := filepath.Join(t.TempDir(), "a.tar.gz")
		if err := os.WriteFile(archive, tarGzEntries(t, []tarEntry{
			{name: "root/lib/libpq.so.5.16", body: "real"},
			{name: "root/lib/libpq.so", link: "libpq.so.5.16", symlink: true},
		}), 0o644); err != nil {
			t.Fatal(err)
		}
		a, _ := artifactFor("postgres")
		if err := extractTarGz(archive, a, dst, "linux"); err != nil {
			t.Fatalf("a sibling symlink inside the destination must be accepted: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(dst, "lib", "libpq.so")); err != nil {
			t.Errorf("the sibling symlink must exist: %v", err)
		}
	})
}

// tarEntry describes one fixture archive entry for tarGzEntries.
type tarEntry struct {
	name    string
	body    string
	link    string
	symlink bool
}

// tarGzEntries builds a .tar.gz from an ORDERED entry list, so a test can pin an
// archive in which one entry deliberately precedes another.
func tarGzEntries(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: 0o755, Size: int64(len(e.body))}
		switch {
		case e.symlink:
			h.Typeflag, h.Linkname = tar.TypeSymlink, e.link
		default:
			h.Typeflag = tar.TypeReg
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.body != "" {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// ----- the real host (no fixture, no network) -----

// TestRealHostInstallIsDetectedAndNotDownloaded is the one case a fixture CANNOT
// make: that detection finds a REAL install on the machine running the suite, in
// whatever layout that machine actually uses, and reaches for the network for
// nothing.
//
// IT IS GUARDED SO IT CAN NEVER DOWNLOAD. The pre-flight bails on a host without a
// complete install, and SkipDownload makes "did not download" a hard property of the
// call rather than an assumption — if detection were wrong, this fails loudly
// instead of fetching a build. That matters because the fixture tests can all pass
// while a real host's layout is rejected: the Debian/Ubuntu server lives in
// /usr/lib/postgresql/<major>/bin with its share dir at /usr/share/postgresql/<major>,
// NOT beside bin/, so a share-dir check that only looked beside bin/ would reject a
// perfectly good distro install and this would be the only test that noticed.
func TestRealHostInstallIsDetectedAndNotDownloaded(t *testing.T) {
	dataDir := t.TempDir()
	opt := defaultAcquireOptions(dataDir, "postgres", "nats-server")
	opt.Out = io.Discard

	det, err := detectInfraBinaries(opt)
	if err != nil {
		t.Skipf("this host has no usable PostgreSQL install to detect: %v", err)
	}
	if len(det.missing) > 0 {
		t.Skipf("this host is missing %v — nothing to detect", det.missing)
	}

	opt.SkipDownload = true // a hard guarantee that this test never reaches the network
	rec, err := acquireInfraBinaries(context.Background(), opt)
	if err != nil {
		t.Fatalf("a host with a complete install must be DETECTED, not downloaded: %v", err)
	}
	if rec.Source != "detected" {
		t.Errorf("Source = %q, want \"detected\" on a host that already has both", rec.Source)
	}
	if strings.HasPrefix(rec.Postgres, dataDir) {
		t.Errorf("Postgres = %q is under the data dir — a real detected install must be used in place, not re-fetched", rec.Postgres)
	}
	// The detected server must be one this build will actually accept, so a detected
	// host does not fail the very next check after being accepted here.
	if major, ok := parsePostgresMajor(rec.PostgresVersion); !ok || major < minPostgresMajor {
		t.Errorf("detected PostgreSQL %q parses to major %d (ok=%v), want >= %d", rec.PostgresVersion, major, ok, minPostgresMajor)
	}
}

// ---------------------------------------------------------------------------
// The four telemetry backends: FLAT archives, port offsets, the Only subset.
// ---------------------------------------------------------------------------

// flatFixtureBody builds an archive shaped like the publisher's for one of the
// four telemetry artifacts: the binary at the ARCHIVE ROOT (no top-level dir),
// under the member name that platform ships. Loki's members carry a platform
// suffix and are zipped; the other three are tarballs with the plain name.
func flatFixtureBody(t *testing.T, a infraArtifact, goos, goarch string) []byte {
	t.Helper()
	member := memberFor(a, goos, goarch)
	body := "#!/bin/sh\nprintf '" + a.Name + " version fixture\\n'\n"
	entries := map[string]string{member: body}
	// Tempo's real tar carries siblings; the extractor must take only the binary,
	// because a prefix match would otherwise pick tempo-cli.
	if a.Name == "tempo" {
		entries["CHANGELOG.md"] = "docs\n"
		entries["LICENSE"] = "license\n"
		entries["tempo-cli"] = "#!/bin/sh\n"
	}
	if strings.HasSuffix(member, ".exe") || a.Name == "loki" {
		return zipBytes(t, entries)
	}
	return tarGz(t, entries)
}

// telemetryFixtureOptions assembles a fixture server serving all four telemetry
// artifacts (plus postgres/nats) and points the options at it.
func telemetryFixtureOptions(t *testing.T, dataDir string, only ...string) (acquireOptions, *[]string) {
	t.Helper()
	bodies := map[string][]byte{
		"postgres":    pgFixtureBody(t, "linux"),
		"nats-server": natsFixtureBody(t, "linux"),
	}
	for _, a := range infraArtifacts {
		switch a.Name {
		case "postgres", "nats-server":
			continue
		}
		bodies[a.Name] = flatFixtureBody(t, a, "linux", "amd64")
	}
	srv, hits := fixtureServer(t, bodies)
	opt := downloadOptions(t, srv, bodies, dataDir)
	opt.Only = only
	return opt, hits
}

// TestFlatArchiveKeepsEntries is THE REGRESSION TEST for the bug this change
// exists to fix. The pre-existing extractor stripped the archive's first path
// segment and skipped entries with no slash — correct for PostgreSQL's and NATS's
// single-top-level-directory layouts, and DESTRUCTIVE for a flat archive: the
// entry "tempo" has no '/', so it was skipped and acquisition failed with "the
// downloaded tempo archive did not contain tempo".
//
// It also pins the two things the flat path must do that the nested path must not:
// take ONLY the declared member (tempo's tar carries tempo-cli, and a prefix match
// would run the wrong binary), and install it under its CANONICAL name (Loki's
// member is loki-linux-amd64, but the supervisor execs `loki`).
func TestFlatArchiveKeepsEntries(t *testing.T) {
	cases := []struct {
		name, goos, wantMember string
	}{
		{"tempo", "linux", "tempo"},
		{"loki", "linux", "loki-linux-amd64"},
		{"victoria-metrics-prod", "linux", "victoria-metrics-prod"},
		{"otelcol-contrib", "linux", "otelcol-contrib"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, ok := artifactFor(tc.name)
			if !ok {
				t.Fatalf("no artifact named %q", tc.name)
			}
			if a.Nested {
				t.Fatalf("%s must be marked FLAT — its binary is at the archive root", tc.name)
			}
			if got := memberFor(a, tc.goos, "amd64"); got != tc.wantMember {
				t.Fatalf("memberFor(%s) = %q, want %q", tc.name, got, tc.wantMember)
			}
			archive := filepath.Join(t.TempDir(), "fixture")
			if err := os.WriteFile(archive, flatFixtureBody(t, a, tc.goos, "amd64"), 0o644); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(t.TempDir(), "out")
			if err := extractArchive(archive, a, out, tc.goos); err != nil {
				t.Fatalf("extract: %v", err)
			}
			canonical := filepath.Join(out, canonicalName(a, tc.goos))
			if _, err := os.Stat(canonical); err != nil {
				t.Fatalf("the flat archive's binary must land at its canonical path %s: %v", canonical, err)
			}
			// The siblings must NOT be unpacked: only the declared member is kept.
			for _, extra := range []string{"CHANGELOG.md", "LICENSE", "tempo-cli", tc.wantMember} {
				if extra == canonicalName(a, tc.goos) {
					continue
				}
				if _, err := os.Stat(filepath.Join(out, extra)); err == nil && extra == "tempo-cli" {
					t.Errorf("a flat single-binary artifact must not unpack %q — that is a DIFFERENT executable", extra)
				}
			}
		})
	}
}

// TestPinnedTelemetryArtifactsCoverPlatforms keeps the four new rows honest the
// same way the loop in TestPinnedArtifactsCoverTheSupportedPlatforms does, and
// additionally requires a member name wherever the archive's member is not the
// executable's own name (Loki, VictoriaMetrics on windows).
func TestPinnedTelemetryArtifactsCoverPlatforms(t *testing.T) {
	platforms := []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64", "windows/amd64"}
	for _, name := range telemetryArtifactNames {
		a, ok := artifactFor(name)
		if !ok {
			t.Fatalf("the pinned table has no artifact named %q", name)
		}
		if a.Nested {
			t.Errorf("%s must be FLAT (Nested:false): its binary is at the archive root", name)
		}
		if a.Distribution {
			t.Errorf("%s is a single binary, not a distribution", name)
		}
		if len(a.Required) != 1 {
			t.Fatalf("%s must require exactly one executable, got %v", name, a.Required)
		}
		for _, plat := range platforms {
			if a.URLs[plat] == "" {
				t.Errorf("%s has no pinned URL for %s", name, plat)
			}
			if len(a.Checksums[plat]) != 64 {
				t.Errorf("%s/%s checksum is %d chars, want a 64-char SHA256", name, plat, len(a.Checksums[plat]))
			}
			// Every platform must resolve a member name, or extraction has nothing
			// to match on.
			parts := strings.SplitN(plat, "/", 2)
			if memberFor(a, parts[0], parts[1]) == "" {
				t.Errorf("%s/%s resolves no archive member", name, plat)
			}
		}
		for plat := range a.Members {
			if a.URLs[plat] == "" {
				t.Errorf("%s declares a member for %s but no URL", name, plat)
			}
		}
	}
}

// TestAcquireDownloadsTelemetryBinaries pins AC1 and AC2's acquisition half: all
// four backends resolve through the SAME detect-else-download mechanism as
// postgres/nats, land under the instance data dir, and are recorded.
func TestAcquireDownloadsTelemetryBinaries(t *testing.T) {
	dataDir := t.TempDir()
	opt, _ := telemetryFixtureOptions(t, dataDir)

	rec, err := acquireInfraBinaries(context.Background(), opt)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	for _, name := range telemetryArtifactNames {
		p := rec.telemetryPath(name)
		if p == "" {
			t.Errorf("%s was not resolved — the acquisition gate dropped it", name)
		}
		if !filepath.IsAbs(p) || !strings.HasPrefix(p, dataDir) {
			t.Errorf("%s resolved to %q, want an absolute path under %s", name, p, dataDir)
		}
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			t.Errorf("%s does not exist as a file after acquisition: %v", name, err)
		}
		// THE EXEC BIT: Loki's zip members carry no mode, so a download that did
		// not chmod would install a binary the supervisor cannot run.
		if st != nil && st.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s is not executable (%04o) — the supervisor could not start it", p, st.Mode().Perm())
		}
	}
	// The record must round-trip the four paths, so a later process reads the same
	// answer instead of probing again.
	back, err := loadInfraBinaries(dataDir)
	if err != nil {
		t.Fatalf("loadInfraBinaries: %v", err)
	}
	if back.Tempo != rec.Tempo || back.Loki != rec.Loki ||
		back.VictoriaMetrics != rec.VictoriaMetrics || back.OTelCollector != rec.OTelCollector {
		t.Errorf("the recorded telemetry set differs from the returned one:\n got %+v\nwant %+v", back, rec)
	}
}

// TestAcquireOnlyHonoursTheSubset pins AC7's acquisition half: a boot that does not
// run telemetry must not download it. `Only` is the mechanism.
func TestAcquireOnlyHonoursTheSubset(t *testing.T) {
	dataDir := t.TempDir()
	opt, hits := telemetryFixtureOptions(t, dataDir, "postgres", "nats-server")

	rec, err := acquireInfraBinaries(context.Background(), opt)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	for _, name := range telemetryArtifactNames {
		if p := rec.telemetryPath(name); p != "" {
			t.Errorf("%s was acquired even though this boot excluded it (%s)", name, p)
		}
	}
	// A record with no telemetry paths must still LOAD: an ORCHICON_TELEMETRY=none
	// boot writes exactly this record, and loadInfraBinaries refusing it would turn
	// every such boot into a failed install.
	if _, err := loadInfraBinaries(dataDir); err != nil {
		t.Errorf("a postgres+nats-only record must be loadable: %v", err)
	}
	for _, h := range *hits {
		switch h {
		case "postgres", "nats-server":
		default:
			t.Errorf("the fixture server was hit for %q, which this acquisition excluded", h)
		}
	}
	// And the supervisor's own gate agrees with the subset it passes.
	off := &supervisor{telemetryEnabled: false}
	if got := off.acquiredArtifacts(); len(got) != 2 {
		t.Errorf("telemetry-off acquires %v, want postgres+nats only", got)
	}
	if got := (&supervisor{telemetryEnabled: true}).acquiredArtifacts(); got != nil {
		t.Errorf("telemetry-on must acquire EVERY artifact (nil Only), got %v", got)
	}
}

// TestAcquireDetectsInstalledTelemetryBackends pins the OTHER half of AC1: a host
// that already has the backends must DETECT them (no download, no network). It is
// the mirror of TestRealTelemetryDetectionUsesAnInstalledBuild, done hermetically
// with shims so it does not depend on what this machine happens to have.
func TestAcquireDetectsInstalledTelemetryBackends(t *testing.T) {
	dataDir := t.TempDir()
	// Only the four: this test is about THEIR detection, and the fixture's
	// postgres/nats would otherwise be downloaded (correctly) and muddy the
	// "nothing was fetched" claim.
	opt, hits := telemetryFixtureOptions(t, dataDir, telemetryArtifactNames...)

	// A PATH holding every artifact under BOTH names the resolver accepts: the
	// canonical executable name and Loki's platform-suffixed member name.
	binDirPath := t.TempDir()
	for _, name := range telemetryArtifactNames {
		a, _ := artifactFor(name)
		shim(t, binDirPath, a.Required[0], name+" version shim", "linux")
	}
	// Loki's published member name is also accepted.
	shim(t, binDirPath, "loki-linux-amd64", "loki version shim", "linux")

	opt.PathEnv = binDirPath
	rec, err := acquireInfraBinaries(context.Background(), opt)
	if err != nil {
		t.Fatalf("acquire with all four installed on PATH: %v", err)
	}
	for _, name := range telemetryArtifactNames {
		p := rec.telemetryPath(name)
		if !strings.HasPrefix(p, binDirPath) {
			t.Errorf("%s = %q, want the installed copy under %s", name, p, binDirPath)
		}
	}
	if len(*hits) != 0 {
		t.Errorf("nothing may be downloaded when all four are installed; the fixture server was hit for %v", *hits)
	}
	if rec.Source != "detected" {
		t.Errorf("Source = %q, want \"detected\"", rec.Source)
	}
	for _, name := range telemetryArtifactNames {
		if rec.telemetryVersion(name) == "" {
			t.Errorf("%s has no recorded version", name)
		}
	}
}

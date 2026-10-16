package main

// services_acquire.go — get a usable PostgreSQL and NATS onto a HOST that has
// neither, so `orchicon services up` (and therefore `make run`, `make migrate`
// and the DB-backed tests) works with no container engine and no system
// Postgres installed.
//
// WHY THIS EXISTS AT ALL. Every other path to Postgres in this repo assumes a
// container: `scripts/container.sh up` publishes 5432 from the instance
// container, and the container supervisor's services-only mode runs the
// postgres that image already carries. The moment the container is taken out of
// the inner loop, "run the plane from source" has no database — and the failure
// is not loud, it is a connection refused from a plane that started fine.
//
// THE SHAPE OF THE ACQUISITION:
//
//   - a HOST install always wins. If `initdb` and `postgres` are both reachable
//     (an operator who has PostgreSQL installed, a distro package, or the
//     Postgres shipped in this very image's /usr/lib/postgresql/<ver>/bin via
//     pg_config) we use THAT directory and download nothing. Duplicating a
//     working install would waste 40 MiB and, worse, create a second cluster the
//     operator does not know about.
//   - otherwise the pinned tarballs are downloaded into
//     <dataDir>/services/{pg,nats} and the SHA256 is VERIFIED before anything is
//     unpacked, mirroring `make tools`' pin-then-verify rule for buf/atlas.
//   - the WHOLE postgres tree is unpacked (bin + lib + share), NOT just the
//     binaries: the release binaries carry a `$ORIGIN/../lib` RUNPATH and the
//     `share/` tree that `initdb` copes from. Copying only `bin/` — or symlinking
//     the binaries elsewhere — produces a binary that dies with
//     "postgres: not found" / "could not open ... postgres.bki" (measured,
//     both).
//
// THIS FILE IS THE SEAM the sibling binary-acquisition work plugs into:
// ensureServiceBinaries is the single function that decides WHERE the services'
// binaries come from, and it is deliberately free of any supervisor state.

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Pinned versions of the two services the inner loop needs.
//
// THEY ARE PINNED, NOT "latest", for the same reason `make tools` pins buf and
// atlas: a floating version makes a developer's failure unreproducible and makes
// the SHA check below meaningless.
const (
	postgresPinnedVersion = "16.6.0"
	natsPinnedVersion     = "2.11.6"
)

// pgReleaseURL is the upstream (theseus-rs) release asset for an arch. The
// target triple is the upstream spelling, not Go's GOARCH, so it is mapped
// rather than derived.
func pgReleaseURL(arch string) (string, bool) {
	target := map[string]string{
		"amd64": "x86_64",
		"arm64": "aarch64",
	}[arch]
	if target == "" {
		return "", false
	}
	return fmt.Sprintf(
		"https://github.com/theseus-rs/postgresql-binaries/releases/download/%s/postgresql-%s-%s-unknown-linux-gnu.tar.gz",
		postgresPinnedVersion, postgresPinnedVersion, target), true
}

// pgReleaseSHA256 is the pinned digest of the archive pgReleaseURL returns,
// per arch. Both were fetched and hashed when this file was written.
func pgReleaseSHA256(arch string) (string, bool) {
	switch arch {
	case "amd64":
		return "f05dc63d918eac05a5dff7f8e0c5a72f89176efda87e1556a6ec1758046c1987", true
	case "arm64":
		return "ae3e4bd7f3907ecf464c9ea50e924106cc5ebe1fe5eb70a1554f6e14fcd59d06", true
	}
	return "", false
}

// natsReleaseURL / natsReleaseSHA256 are the same pair for the NATS server,
// whose release arch names DO match GOARCH.
func natsReleaseURL(arch string) (string, bool) {
	if arch != "amd64" && arch != "arm64" {
		return "", false
	}
	return fmt.Sprintf(
		"https://github.com/nats-io/nats-server/releases/download/v%s/nats-server-v%s-linux-%s.tar.gz",
		natsPinnedVersion, natsPinnedVersion, arch), true
}

func natsReleaseSHA256(arch string) (string, bool) {
	switch arch {
	case "amd64":
		return "5e5272dd1bdb8da020aedc8cf883fb26c128441809a992f156e0b1ccb28ac5b7", true
	case "arm64":
		return "4fe3c40bddc58e31012f55865df0dcd24012995d2893a4d64a0ae5cb147d7a36", true
	}
	return "", false
}

// serviceBinaries is where the host services supervisor finds its executables.
type serviceBinaries struct {
	// PG is the postgres bindir: the directory holding postgres/initdb/psql.
	PG string
	// NATS is the nats-server executable itself (not a directory — the release
	// ships one binary, and a bindir with a single entry is noise).
	NATS string
	// Source says where they came from ("host" or the downloaded dir), so `up`
	// can report it: an operator who wonders which postgres their instance is
	// using gets the answer without reading this code.
	Source string
}

// ensureServiceBinaries locates (or fetches) postgres and nats-server for the
// host services supervisor.
//
// IDEMPOTENT BY CONSTRUCTION: a host install that exists is returned as-is, and
// an already-unpacked download is detected by the presence of its binaries, so a
// second call neither downloads nor unpacks. That is what makes `services up`
// safe to run from a Makefile prerequisite on every `make run`.
func ensureServiceBinaries(dataDir string) (serviceBinaries, error) {
	if pg := hostPostgresBinDir(); pg != "" {
		if nats := hostNATSBin(); nats != "" {
			return serviceBinaries{PG: pg, NATS: nats, Source: "host PATH"}, nil
		}
	}

	servicesDir := filepath.Join(dataDir, "services")
	pgRoot := filepath.Join(servicesDir, "pg")
	natsRoot := filepath.Join(servicesDir, "nats")

	pgBin := filepath.Join(pgRoot, "bin")
	if !fileExists(filepath.Join(pgBin, "initdb")) || !fileExists(filepath.Join(pgBin, "postgres")) {
		arch := runtime.GOARCH
		url, ok := pgReleaseURL(arch)
		if !ok {
			return serviceBinaries{}, fmt.Errorf("no pinned PostgreSQL build for %s/%s — install PostgreSQL and put initdb on PATH, or set ORCHICON_PG_BINDIR", runtime.GOOS, arch)
		}
		sha, _ := pgReleaseSHA256(arch)
		archive := filepath.Join(servicesDir, "downloads", "postgres-"+postgresPinnedVersion+".tar.gz")
		if err := fetchVerified(url, sha, archive); err != nil {
			return serviceBinaries{}, fmt.Errorf("acquire PostgreSQL %s: %w", postgresPinnedVersion, err)
		}
		if err := unpackTarGz(archive, pgRoot, 1, nil); err != nil {
			return serviceBinaries{}, fmt.Errorf("unpack PostgreSQL: %w", err)
		}
	}

	natsBin := filepath.Join(natsRoot, "bin", "nats-server")
	if !fileExists(natsBin) {
		arch := runtime.GOARCH
		url, ok := natsReleaseURL(arch)
		if !ok {
			return serviceBinaries{}, fmt.Errorf("no pinned nats-server build for %s/%s — install nats-server and put it on PATH", runtime.GOOS, arch)
		}
		sha, _ := natsReleaseSHA256(arch)
		archive := filepath.Join(servicesDir, "downloads", "nats-server-"+natsPinnedVersion+".tar.gz")
		if err := fetchVerified(url, sha, archive); err != nil {
			return serviceBinaries{}, fmt.Errorf("acquire nats-server %s: %w", natsPinnedVersion, err)
		}
		// Keep ONLY the server binary from the release: it also ships a LICENSE
		// and README, and the archive's top dir is version-named, so a flat
		// single-file copy is both smaller and stable across version bumps.
		if err := unpackTarGz(archive, natsRoot, 1, func(name string) bool {
			return filepath.Base(name) == "nats-server"
		}); err != nil {
			return serviceBinaries{}, fmt.Errorf("unpack nats-server: %w", err)
		}
	}

	return serviceBinaries{PG: pgBin, NATS: natsBin, Source: filepath.Join(dataDir, "services")}, nil
}

// hostPostgresBinDir returns the directory of a PostgreSQL install already
// present on this host, or "".
//
// THREE PROBES, in order of how deliberate they are:
//
//  1. ORCHICON_PG_BINDIR — an explicit answer always wins, and it is also the
//     knob `scripts/check-rls.sh` reuses to find psql for the same install.
//  2. a PATH lookup for `initdb` (the one binary a runnable cluster cannot do
//     without), so a developer whose distro put postgres on PATH is served from
//     PATH rather than from a redundant download.
//  3. `pg_config --bindir`, because Debian/Ubuntu place the server binaries under
//     /usr/lib/postgresql/<ver>/bin and put only the CLIENT tools on PATH — that
//     is exactly the layout of the container this code also runs in, and without
//     this rung it would be the one host shape that silently downloads a second
//     PostgreSQL.
//
// EVERY probe must yield BOTH initdb and postgres: a bindir with one but not the
// other is a broken install, and handing it back would fail later with a much
// worse error.
func hostPostgresBinDir() string {
	candidates := []string{os.Getenv("ORCHICON_PG_BINDIR")}
	if p, err := exec.LookPath("initdb"); err == nil {
		candidates = append(candidates, filepath.Dir(p))
	}
	if out, err := exec.Command("pg_config", "--bindir").Output(); err == nil {
		candidates = append(candidates, strings.TrimSpace(string(out)))
	}
	for _, dir := range candidates {
		if dir == "" {
			continue
		}
		if fileExists(filepath.Join(dir, "initdb")) && fileExists(filepath.Join(dir, "postgres")) {
			return dir
		}
	}
	return ""
}

// hostNATSBin returns the path of a nats-server already on PATH, or "".
//
// PATH ONLY, deliberately: NATS ships as a single static binary with no data
// directory and no supporting tree, so "it is on PATH" is the entire test for
// whether the host already has one.
func hostNATSBin() string {
	p, err := exec.LookPath("nats-server")
	if err != nil {
		return ""
	}
	return p
}

// fetchVerified downloads url into dest, refusing anything whose SHA256 is not
// sha256hex.
//
// THE DIGEST IS CHECKED BEFORE THE FILE IS PUT IN PLACE, and that order is the
// whole point: a truncated or substituted download must never become the
// postgres the developer runs. It is the Go twin of the pin-then-verify rule
// `make tools` applies to buf and atlas.
//
// The download goes to a TEMP name in the destination directory and is renamed
// only after it verifies, so an interrupted run leaves no half-file that a later
// run would happily treat as a cached artifact.
//
// An already-present, already-verified dest is a no-op, which is what makes the
// Makefile prerequisite cheap on the second `make run`.
func fetchVerified(url, sha256hex, dest string) error {
	if fileExists(dest) {
		got, err := fileSHA256(dest)
		if err == nil && strings.EqualFold(got, sha256hex) {
			return nil
		}
		// A present-but-wrong archive is a previous partial or stale download:
		// remove it rather than fail forever on a name collision.
		_ = os.Remove(dest)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}

	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dest), ".download-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once the rename below succeeds

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		tmp.Close()
		return fmt.Errorf("download %s: %w", url, err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, sha256hex) {
		return fmt.Errorf("checksum mismatch for %s: want %s, got %s (refusing to install it)", url, sha256hex, got)
	}
	return os.Rename(tmpName, dest)
}

// unpackTarGz extracts a .tar.gz, dropping `strip` leading path components and
// keeping only entries for which keep (when non-nil) returns true.
//
// Two things are enforced rather than assumed:
//
//   - NO PATH ESCAPE. Every entry is resolved and checked to stay under dest, so
//     a crafted archive cannot write outside the services directory.
//   - the EXECUTABLE BIT is preserved from the archive header, because a
//     non-executable `postgres` fails with "permission denied" — the least
//     informative error this path could produce.
func unpackTarGz(archive, dest string, strip int, keep func(string) bool) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := stripComponents(hdr.Name, strip)
		if name == "" {
			continue
		}
		if keep != nil && !keep(hdr.Name) && hdr.Typeflag != tar.TypeDir {
			continue
		}
		target := filepath.Join(dest, name)
		if target != dest && !strings.HasPrefix(target, dest+string(os.PathSeparator)) {
			return fmt.Errorf("archive entry %q escapes the destination", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(hdr.Mode) & 0o777
			if mode == 0 {
				mode = 0o644
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// Symlinks inside the postgres tree point at siblings (lib/*.so,
			// share/*), so they are reproduced only when they too stay inside.
			linkTarget := filepath.Join(filepath.Dir(target), hdr.Linkname)
			if !strings.HasPrefix(linkTarget, dest+string(os.PathSeparator)) {
				continue
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		default:
			// Hard links, devices, fifos: not present in these releases and not
			// wanted. Skipping is deliberate — a device node from a tarball is a
			// privilege escalation, not a feature.
		}
	}
}

// stripComponents removes n leading path components from an archive entry name.
func stripComponents(name string, n int) string {
	parts := strings.Split(filepath.ToSlash(strings.TrimPrefix(name, "./")), "/")
	// A trailing slash yields an empty final element; drop it so the join below
	// does not produce a trailing separator.
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	if n >= len(parts) {
		return ""
	}
	return filepath.Join(parts[n:]...)
}

// fileSHA256 returns the hex digest of a file.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fileExists reports whether path is a regular file (or a symlink to one).
func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

package worktree

// scratch.go — PER-SESSION SCRATCH, AND ITS REMOVAL.
//
// DefaultScratchDir (/tmp/orchicon) is the one writable area outside the project that workers and the
// Ask suite may use. Sandboxed bash redirects XDG_DATA_HOME into it so a worker's tests cannot reach the
// plane's live stores (internal/orchicon/hosttools.go sandboxedBashEnviron). The redirect has a side effect:
// podman derives its image store from XDG_DATA_HOME, so every `podman build` run through those tools
// created a full image store inside the scratch area. On a host where /tmp is a tmpfs that is RAM: one
// machine held 34.7 GB there (57 images, 0 containers) and nothing ever removed it.
//
// So scratch is now PER SESSION and has a lifecycle:
//
//	<scratch>/sessions/<id>/xdg   a session's XDG_DATA_HOME (its private podman store lives here)
//	<scratch>/sessions/<id>/data  its ORCHICON_DATA_DIR
//
// A native execution owns one session and removes it when it ends (RemoveSessionDir). An Ask conversation
// is long-lived and has no end event, and a crashed plane never reaches an end event at all, so
// SweepSessions removes sessions that have been idle for ScratchSessionTTL; it also clears the old SHARED
// layout (<scratch>/xdg, <scratch>/data) that earlier versions left behind.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/procwin"
)

// SessionsDirName is the directory under the scratch root that holds one subdirectory per session.
const SessionsDirName = "sessions"

// ScratchSessionTTL is how long a session's scratch may sit unused before the sweeper removes it. An
// execution removes its own on exit, so this bounds only conversation-lived and crash-orphaned scratch.
const ScratchSessionTTL = 6 * time.Hour

// legacyScratchDirs are the SHARED directories older versions used directly under the scratch root.
var legacyScratchDirs = []string{"xdg", "data"}

// sessionIdleDepth bounds how deep the idle check looks for the newest modification. A podman store is
// <session>/xdg/containers/storage/<layer dirs>: new layers touch directories at depth 4-5, which is
// enough to show a build is in progress without walking a million overlay files.
const sessionIdleDepth = 5

// NewSessionID returns a fresh random scratch session id.
func NewSessionID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// SanitizeSessionID keeps an id usable as ONE path element: [A-Za-z0-9_-], at most 64 bytes, and never empty.
func SanitizeSessionID(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
		if b.Len() >= 64 {
			break
		}
	}
	if s := strings.Trim(b.String(), "-"); s != "" {
		return s
	}
	return "anon"
}

// SessionRoot is the directory that holds every session's scratch.
func SessionRoot(scratch string) string { return filepath.Join(scratch, SessionsDirName) }

// SessionDir is one session's private scratch directory (not created here).
func SessionDir(scratch, id string) string {
	return filepath.Join(SessionRoot(scratch), SanitizeSessionID(id))
}

// RemoveSessionDir removes one session's scratch. It refuses anything that is not directly inside the
// scratch root's sessions directory, so a wrong argument cannot delete a project.
func RemoveSessionDir(scratch, dir string) error {
	if filepath.Clean(filepath.Dir(dir)) != filepath.Clean(SessionRoot(scratch)) {
		return fmt.Errorf("refusing to remove %q: not a session directory under %q", dir, SessionRoot(scratch))
	}
	return removeTree(dir)
}

// removeTree deletes a scratch tree, including a ROOTLESS PODMAN STORE inside it. Such a store holds files
// owned by sub-uids of the user namespace, which a plain os.RemoveAll cannot unlink ("permission denied" —
// measured: 538 paths left behind after one build). Podman removes its own store: containers, images (which
// takes the layers with them) and volumes, run against the scratch store through the same XDG_DATA_HOME
// redirect that created it. What remains is empty directories the caller owns, and os.RemoveAll takes those.
//
// NOT `podman system reset`: it also clears the RUN root, which the redirect does not move — it is shared
// with the operator's real store.
func removeTree(dir string) error {
	if _, err := os.Lstat(dir); os.IsNotExist(err) {
		return nil
	}
	var podmanNote string
	if runtime.GOOS != "windows" {
		xdg := filepath.Join(dir, "xdg")
		if _, err := os.Stat(filepath.Join(xdg, "containers")); err == nil {
			if podman, lerr := exec.LookPath("podman"); lerr == nil {
				env := append(os.Environ(), "XDG_DATA_HOME="+xdg)
				for _, args := range [][]string{
					{"rm", "--all", "--force", "--ignore"},
					{"rmi", "--all", "--force"},
					{"volume", "rm", "--all", "--force"},
				} {
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
					cmd := procwin.CommandContext(ctx, podman, args...)
					cmd.Env = env
					if out, rerr := cmd.CombinedOutput(); rerr != nil {
						podmanNote = fmt.Sprintf("podman %s: %v: %s", strings.Join(args, " "), rerr, strings.TrimSpace(string(out)))
					}
					cancel()
				}
			}
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		if podmanNote != "" {
			return fmt.Errorf("%w (%s)", err, podmanNote)
		}
		return err
	}
	return nil
}

// newestModification returns the most recent mtime among dir and its entries down to sessionIdleDepth.
func newestModification(dir string) time.Time {
	var newest time.Time
	base := filepath.Clean(dir)
	_ = filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		if d.IsDir() {
			rel, _ := filepath.Rel(base, p)
			if rel != "." && strings.Count(rel, string(filepath.Separator)) >= sessionIdleDepth-1 {
				return filepath.SkipDir
			}
		}
		return nil
	})
	return newest
}

// SweepSessions removes session scratch (and the legacy shared layout) that nothing has touched for maxAge,
// and returns how many directories it removed. A directory in active use is never idle: a build writing
// layers keeps its mtime fresh. Failures are logged and skipped, never fatal.
func SweepSessions(scratch string, maxAge time.Duration, now time.Time, log *slog.Logger) int {
	if log == nil {
		log = slog.Default()
	}
	var candidates []string
	if entries, err := os.ReadDir(SessionRoot(scratch)); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				candidates = append(candidates, filepath.Join(SessionRoot(scratch), e.Name()))
			}
		}
	}
	for _, name := range legacyScratchDirs {
		p := filepath.Join(scratch, name)
		if fi, err := os.Lstat(p); err == nil && fi.IsDir() {
			candidates = append(candidates, p)
		}
	}
	removed := 0
	for _, dir := range candidates {
		newest := newestModification(dir)
		if newest.IsZero() || now.Sub(newest) < maxAge {
			continue
		}
		if err := removeTree(dir); err != nil {
			log.Warn("sandbox scratch sweep: could not remove an idle scratch directory", "dir", dir, "error", err)
			continue
		}
		removed++
	}
	return removed
}

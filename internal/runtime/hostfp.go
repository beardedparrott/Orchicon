package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
)

// Host-input fingerprinting for the warm pool.
//
// A runtime container caches, at create time, a set of read-once HOST
// artifacts: the opencode config (~/.config/opencode/opencode.json(c)), the
// provider auth (~/.local/share/opencode/auth.json, copied into the ephemeral
// XDG dir at container start), the mounted adapter CLI install
// (~/.opencode), and the resolved GH token. The pool folds a fingerprint of
// these into the environment key (see poolEnvKey) so ANY change to a read-once
// host input forces the next checkout to create a fresh container instead of
// silently reusing a warm one serving the stale value.
//
// The fingerprint is computed daemon-side from d.HostHome — the same trust
// domain as the mounts — and is gated on the SAME existence checks
// createContainer uses (daemon.go:369-406), so the key always agrees with what
// is actually mounted into the container. No secret content ever appears in
// the key or the logs: config/auth feed a digest, and the GH token feeds a
// length+prefix digest only.

// hashFileContent returns the hex SHA-256 of a file's bytes. A file that
// exists but cannot be read contributes a distinct sentinel (never an empty
// "absent" value), so a permission/read failure changes the fingerprint →
// pool miss → fresh container. Failing toward a fresh container is the safe
// direction: a container created under a config it could not read is
// suspect.
func hashFileContent(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "unreadable:" + path
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ghTokenFingerprint fingerprints the resolved GH token WITHOUT exposing it:
// only the token LENGTH and its first 12 characters feed the digest, so any
// rotation (different length or prefix) changes the fingerprint while the full
// token never appears in the key or any log. Returns "" for an empty token (no
// GH_TOKEN in the container — nothing to fingerprint).
func ghTokenFingerprint(tok string) string {
	if tok == "" {
		return ""
	}
	prefix := tok
	if len(prefix) > 12 {
		prefix = prefix[:12]
	}
	sum := sha256.Sum256([]byte("gh:" + strconv.Itoa(len(tok)) + ":" + prefix))
	return hex.EncodeToString(sum[:])
}

// maxAdapterFingerprintEntries bounds the adapter-install stat walk so a
// pathological tree (a huge node_modules) cannot balloon per-checkout cost.
const maxAdapterFingerprintEntries = 50000

// adapterInstallFingerprint fingerprints the mounted adapter CLI install
// (~/.opencode/bin + ~/.opencode/node_modules) with STAT-ONLY metadata:
// sorted (relpath, size, mtime-ns, mode-type) tuples hashed. No file content
// is read — the opencode binary and provider packages can be large; metadata
// changes on any upgrade, install, or reinstall. Returns "" when neither root
// exists.
func adapterInstallFingerprint(home string) string {
	roots := []string{
		filepath.Join(home, ".opencode", "bin"),
		filepath.Join(home, ".opencode", "node_modules"),
	}
	var entries []string
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(path string, de fs.DirEntry, err error) error {
			if err != nil {
				return nil // missing/unreadable subtree contributes nothing
			}
			info, ierr := de.Info()
			if ierr != nil {
				return nil
			}
			rel, rerr := filepath.Rel(home, path)
			if rerr != nil {
				rel = path
			}
			entries = append(entries, fmt.Sprintf("%s|%d|%d|%d|%t", rel, info.Size(), info.ModTime().UnixNano(), info.Mode()&os.ModeType, de.IsDir()))
			if len(entries) > maxAdapterFingerprintEntries {
				return filepath.SkipAll
			}
			return nil
		})
	}
	if len(entries) == 0 {
		return ""
	}
	sort.Strings(entries)
	h := sha256.New()
	for _, e := range entries {
		_, _ = io.WriteString(h, e+"\n")
	}
	return hex.EncodeToString(h.Sum(nil))
}

// hostInputsFingerprint builds the combined fingerprint of the read-once host
// inputs under home, gated on the SAME existence checks createContainer uses
// for the mounts so the pool key always agrees with what the container
// actually mounts/copies. ghFp is the pre-computed non-sensitive GH-token
// fingerprint (from ghTokenFingerprint). Returns "" when home is empty or no
// fingerprinted input applies — the pool key then reduces to image+mounts
// (unchanged behavior when nothing is mounted from HostHome).
func hostInputsFingerprint(home, ghFp string) string {
	if home == "" {
		return ""
	}
	h := sha256.New()
	added := false
	add := func(label, val string) {
		_, _ = io.WriteString(h, label+"="+val+"\n")
		added = true
	}

	// 1. opencode config — content-hashed (small files; exact). The serve
	// reads it once at boot and caches it in-process.
	cfgDir := filepath.Join(home, ".config", "opencode")
	if st, err := os.Stat(cfgDir); err == nil && st.IsDir() {
		for _, name := range []string{"opencode.json", "opencode.jsonc"} {
			p := filepath.Join(cfgDir, name)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				add("cfg:"+name, hashFileContent(p))
			}
		}
	}

	// 2. provider auth — content-hashed; COPIED into the ephemeral XDG dir at
	// container start, so a rotated/new key never reaches a warm container.
	auth := filepath.Join(home, ".local", "share", "opencode", "auth.json")
	if st, err := os.Stat(auth); err == nil && !st.IsDir() {
		add("auth", hashFileContent(auth))
	}

	// 3. adapter install — stat-only fingerprint of ~/.opencode. The serve
	// execs the binary and loads provider npm packages at start; an upgrade
	// or a newly installed provider package is invisible to warm containers.
	if st, err := os.Stat(filepath.Join(home, ".opencode", "bin", "opencode")); err == nil && !st.IsDir() {
		if fp := adapterInstallFingerprint(home); fp != "" {
			add("adapter", fp)
		}
	}

	// 4. GH token — non-sensitive fingerprint of the resolved token (length +
	// first 12 chars only).
	if ghFp != "" {
		add("gh", ghFp)
	}

	if !added {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

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

	"github.com/beardedparrott/orchicon/internal/adapter"
)

// Host-input fingerprinting for the warm pool.

// hashFileContent returns the hex SHA-256 of a file's bytes.
func hashFileContent(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "unreadable:" + path
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ghTokenFingerprint fingerprints the resolved GH token WITHOUT exposing it: only the token LENGTH
// and its first 12 characters feed the digest.
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

// adapterInstallFingerprint fingerprints every mounted adapter CLI install declared in the install
// table (opencode + claude) with STAT-ONLY metadata: sorted (relpath, size, mtime-ns.
func adapterInstallFingerprint(home string) string {
	roots := adapterFingerprintRoots(home)
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

// adapterFingerprintRoots walks the declared install table (the SAME declaration the daemon mounts
// from) and returns every tree marked fingerprint.
func adapterFingerprintRoots(home string) []string {
	if home == "" {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	for _, kind := range []string{adapter.DefaultAdapterKind, adapter.KindClaude} {
		installs, declared := adapterInstalls(home, kind)
		if !declared {
			continue
		}
		for _, in := range installs {
			for _, root := range in.fingerprint {
				if _, ok := seen[root]; ok {
					continue
				}
				seen[root] = struct{}{}
				out = append(out, root)
			}
		}
	}
	sort.Strings(out)
	return out
}

// hostInputsFingerprint builds the combined fingerprint of the read-once host inputs under home,
// gated on the SAME existence checks createContainer uses for the mounts so the pool key always.
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

	// 3.
	if fp := adapterInstallFingerprint(home); fp != "" {
		add("adapter", fp)
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

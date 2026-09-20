package contextfiles

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// FileStamp is the per-path change fingerprint recorded for a context
// path (ADR-0009 D5). For a file it is a SHA-256 over the path + size +
// first/last 1 KiB of content (cheap and change-sensitive without hashing
// multi-MB files wholesale); for a directory it is a SHA-256 over the
// bounded entry listing + each file's size+mtime. A changed stamp means
// the rendered prefix changes (costing a cache miss) — the diff is logged
// so the operator can correlate a miss with an edit.
type FileStamp struct {
	Path  string
	Stamp string
}

// Fingerprint computes a stable content fingerprint for a resolved
// context-file set. paths is the raw context_files list (files AND
// directories); each is resolved exactly as the renderer resolves it, so
// the fingerprint and the rendered bytes always agree. Returns:
//
//   - fp: the hex SHA-256 fingerprint of the whole set (empty-set → a
//     fixed "none" sentinel so an unset context is still byte-stable).
//   - stamps: the per-path stamps (sorted), for diffing against a prior
//     run's stamps by the cache layer.
//
// A path that cannot be read stamps as "missing:<path>" — identical to
// what the renderer notes — so an unreadable file is stable across runs
// too (no spurious churn on the same error).
func Fingerprint(paths []string, projectDir string) (fp string, stamps []FileStamp, err error) {
	if len(paths) == 0 {
		return "none", nil, nil
	}
	h := sha256.New()
	stamps = make([]FileStamp, 0, len(paths))
	seen := map[string]bool{}
	for _, p := range paths {
		resolved := Resolve(p, projectDir)
		if resolved == "" || seen[resolved] {
			continue
		}
		seen[resolved] = true
		stamp, serr := stampPath(resolved)
		if serr != nil {
			return "", nil, serr
		}
		stamps = append(stamps, FileStamp{Path: resolved, Stamp: stamp})
		fmt.Fprintf(h, "%s\x00%s\n", resolved, stamp)
	}
	sort.Slice(stamps, func(i, j int) bool { return stamps[i].Path < stamps[j].Path })
	return hex.EncodeToString(h.Sum(nil)[:8]), stamps, nil
}

// stampPath is the per-path fingerprint (file or directory).
func stampPath(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "missing:" + path, nil
	}
	if info.IsDir() {
		entries, werr := WalkDir(path, MaxContextFiles)
		if werr != nil {
			return "", fmt.Errorf("stamp directory %q: %w", path, werr)
		}
		sort.Strings(entries)
		h := sha256.New()
		fmt.Fprintf(h, "dir %s\n", path)
		for _, e := range entries {
			st, serr := os.Stat(e)
			if serr != nil {
				fmt.Fprintf(h, "%s missing\n", e)
				continue
			}
			fmt.Fprintf(h, "%s %d %d\n", e, st.Size(), st.ModTime().UnixNano())
		}
		return hex.EncodeToString(h.Sum(nil)[:8]), nil
	}
	if !info.Mode().IsRegular() {
		return "notfile:" + path, nil
	}
	h := sha256.New()
	fmt.Fprintf(h, "file %s %d\n", path, info.Size())
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		return "unreadable:" + path, nil
	}
	// Hash size + a head/tail sample + a mid sample: catches edits
	// anywhere in a large file without hashing megabytes every run.
	n := len(data)
	if n <= 3072 {
		h.Write(data)
	} else {
		h.Write(data[:1024])
		h.Write(data[n/2-512 : n/2+512])
		h.Write(data[n-1024:])
	}
	return hex.EncodeToString(h.Sum(nil)[:8]), nil
}

// DiffStamps returns human-readable lines describing how a set of
// stamps changed from a previous run (added / removed / changed paths),
// used in the prefix-change log so a cache miss can be attributed to a
// specific context-file edit. An empty result means the sets are
// identical.
func DiffStamps(prev, cur []FileStamp) []string {
	if prev == nil {
		prev = []FileStamp{}
	}
	if cur == nil {
		cur = []FileStamp{}
	}
	var changed []string
	pm := make(map[string]string, len(prev))
	for _, s := range prev {
		pm[s.Path] = s.Stamp
	}
	cm := make(map[string]string, len(cur))
	for _, s := range cur {
		cm[s.Path] = s.Stamp
	}
	for _, s := range cur {
		if old, ok := pm[s.Path]; !ok {
			changed = append(changed, fmt.Sprintf("+ %s (new)", s.Path))
		} else if old != s.Stamp {
			changed = append(changed, fmt.Sprintf("~ %s (changed)", s.Path))
		}
	}
	for _, s := range prev {
		if _, ok := cm[s.Path]; !ok {
			changed = append(changed, fmt.Sprintf("- %s (removed)", s.Path))
		}
	}
	sort.Strings(changed)
	return changed
}

// StampsFromJSON rebuilds a stamp list from its JSON encoding (the shape
// the scheduler stores alongside a cached section). Malformed or empty
// input yields nil.
func StampsFromJSON(data []byte) []FileStamp {
	if len(data) == 0 {
		return nil
	}
	var s []FileStamp
	if json.Unmarshal(data, &s) != nil {
		return nil
	}
	return s
}

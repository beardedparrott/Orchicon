// Package repoguard keeps the operator's own checkout from being silently re-pointed at a worker's worktree.
//
// THE INCIDENT, THREE TIMES NOW. A process acting for a run writes `core.worktree = <run worktree>` into the
// project's OWN .git/config. Every later git command in the operator's checkout then reads the WORKER'S files
// against the operator's index: a clean tree shows 20 dirty files, `git checkout develop` refuses ("your local
// changes would be overwritten"), and the plane's own worktree reconciler logs "cannot prune; main repo
// unresolvable" five times a second (5,622 lines in ten minutes) until someone notices.
//
// internal/gitenv documents the mechanism and closes the vectors it found one at a time — scrub the pointers
// from the plane's own git calls, from in-process worker bash, from fixtures. This package is the BACKSTOP for
// the vector nobody has found yet: it does not care which command wrote the setting. It reads the project's
// config directly, and when a platform-originated poisoning is present it UNSETS it, loudly, and says which run
// the value pointed at — which is also the first evidence of who wrote it.
//
// WHAT COUNTS AS POISON is deliberately narrow. A value is only healed when it can ONLY have come from us:
//
//   - core.worktree pointing inside <project>/.orchicon-worktrees/ (the platform's own directory);
//   - user.email == worktree-test@orchicon.dev, the identity our git fixtures use (user.name alongside it only
//     when it is the fixture's too).
//
// A project that sets core.worktree to something else, or has its own identity, is left alone.
package repoguard

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/beardedparrott/orchicon/internal/gitenv"
	"github.com/beardedparrott/orchicon/internal/procwin"
)

// WorktreesDirName is the platform's per-run worktree directory under a project.
const WorktreesDirName = ".orchicon-worktrees"

const (
	fixtureEmail = "worktree-test@orchicon.dev"
	fixtureName  = "worktree-test"
)

// Finding is one poisoned setting.
type Finding struct {
	Key    string `json:"key"`    // "core.worktree", "user.email", "user.name"
	Value  string `json:"value"`  // what it held (the evidence)
	Reason string `json:"reason"` // why it is considered ours
	// Run is the run id the value points at, when it names a run worktree ("" otherwise).
	Run string `json:"run,omitempty"`
}

// parseConfig reads the keys this package cares about from git config text: section.key → last value.
// It understands plain `[section]` headers and ignores subsections ([remote "origin"]), comments and anything
// it cannot parse — a config it cannot read yields no findings, never a false alarm.
func parseConfig(text string) map[string]string {
	out := map[string]string{}
	section := ""
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' {
			end := strings.IndexByte(line, ']')
			if end < 0 {
				section = ""
				continue
			}
			header := strings.TrimSpace(line[1:end])
			if strings.ContainsAny(header, " \t\"") {
				section = "" // a subsection: not one we read
			} else {
				section = strings.ToLower(header)
			}
			continue
		}
		if section == "" {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)
		if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
			val = val[1 : len(val)-1]
		}
		out[section+"."+key] = val
	}
	return out
}

// fold normalises a path for comparison: slashes, and case where the filesystem is case-insensitive in
// practice (Windows/macOS defaults). Folding case everywhere only ever makes the guard see MORE matches inside
// our own directory, which is the safe direction.
func fold(p string) string { return strings.ToLower(filepath.ToSlash(filepath.Clean(p))) }

// Inspect returns the platform-originated poisoning present in a project's git config text.
func Inspect(configText, projectDir string) []Finding {
	cfg := parseConfig(configText)
	var out []Finding

	if wt := cfg["core.worktree"]; wt != "" {
		prefix := fold(filepath.Join(projectDir, WorktreesDirName)) + "/"
		if strings.HasPrefix(fold(wt)+"/", prefix) {
			f := Finding{Key: "core.worktree", Value: wt,
				Reason: "points inside " + WorktreesDirName + ": a run's worktree was recorded as the project's work tree"}
			rest := strings.TrimPrefix(fold(wt)+"/", prefix)
			if id, _, _ := strings.Cut(rest, "/"); id != "" {
				f.Run = strings.ToUpper(id)
			}
			out = append(out, f)
		}
	}
	if strings.EqualFold(cfg["user.email"], fixtureEmail) {
		out = append(out, Finding{Key: "user.email", Value: cfg["user.email"],
			Reason: "the identity our git fixtures use: a test ran `git config` against this repository"})
		if strings.EqualFold(cfg["user.name"], fixtureName) {
			out = append(out, Finding{Key: "user.name", Value: cfg["user.name"], Reason: "the fixture identity's name"})
		}
	}
	return out
}

// Result is what one check found and did.
type Result struct {
	Dir      string
	Findings []Finding
	Healed   bool
	Err      error
}

// configPath is the project's own config file, or "" when the project is not an ordinary checkout (no .git
// directory — e.g. itself a linked worktree, whose .git is a file).
func configPath(projectDir string) string {
	gitDir := filepath.Join(projectDir, ".git")
	if fi, err := os.Stat(gitDir); err != nil || !fi.IsDir() {
		return ""
	}
	return filepath.Join(gitDir, "config")
}

// Check inspects one project and heals any poisoning it finds. It reads and writes the config FILE directly
// (`git config --file`), so it is immune to the very thing it guards against: an ambient GIT_DIR or a poisoned
// core.worktree cannot redirect it.
func Check(ctx context.Context, projectDir string) Result {
	res := Result{Dir: projectDir}
	cfg := configPath(projectDir)
	if cfg == "" {
		return res
	}
	raw, err := os.ReadFile(cfg)
	if err != nil {
		return res // unreadable: not ours to report
	}
	res.Findings = Inspect(string(raw), projectDir)
	if len(res.Findings) == 0 {
		return res
	}
	for _, f := range res.Findings {
		cmd := procwin.CommandContext(ctx, "git", "config", "--file", cfg, "--unset-all", f.Key)
		cmd.Env = gitenv.Clean(os.Environ())
		if out, rerr := cmd.CombinedOutput(); rerr != nil {
			res.Err = fmt.Errorf("unset %s: %v: %s", f.Key, rerr, strings.TrimSpace(string(out)))
			return res
		}
	}
	res.Healed = true
	return res
}

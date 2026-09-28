package protectedpath

// Package protectedpath declares the paths a DESTRUCTIVE operation must never be allowed to equal or
// contain — the path analogue of internal/neverallow's binary class, and enforced at every layer for
// the same reason: "two lists that agree today" agree only until someone edits one.
//
// WHY THIS IS NOT A DENY ENTRY, which is what it looks like it should be. The permission policy's
// deny list matches PATTERNS against the target, so protecting `/home` that way would mean writing
// down the ancestors of the work scope by hand — `/home`, `/home/<user>`, `/home/<user>/projects`,
// … — and those differ on every machine and change whenever the operator opens a different project.
// The author of a policy cannot know them.
//
// And it cannot be expressed as a pattern even in principle: `rm -rf ~/projects` is catastrophic when
// the project is `~/projects/Orchicon` and perfectly reasonable when it is `~/projects/tmp`, so the
// DANGER is not a property of the path. It is a property of the RELATIONSHIP between the path and
// what the session works in. That relationship is computable and a glob is not.
//
// WHAT IT PROTECTS, in two lists, because the two need different rules:
//
//	rm -rf /              equals the filesystem root
//	rm -rf /home          equals the home directory
//	rm -rf ~/projects     contains the project (whatever it is called)
//	rm -rf ~/.local/share/orchicon   equals the plane's own state
//
// THE ONE CASE IT DELIBERATELY DOES NOT REACH: a target that is INSIDE a root but contains nothing
// protected — `rm -rf /tmp/whatever`, or a file in the project. Those are ordinary destructive
// operations, and the consent layer's job is to ask about them.
//
// IT IS NOT WAIVABLE, and that is the point. It sits above FULLSEND at both enforcement layers, in
// the same position as the deny list, because it is the same category of rule: a decision that no
// permission request can turn into an approval. Measured before this existed: with FULLSEND on,
// `rm -rf /home` from a project ran — the sanctioned-set tests are skipped by design, and an
// ancestor of the project is not in any set to begin with, so nothing refused it.

import (
	"os"
	"path/filepath"
	"strings"
)

// Roots returns the MACHINE-LEVEL protected roots for a session or execution: the paths whose
// destruction or re-permissioning can never be what anyone wants, whatever the work is.
//
// These are refused when a target EQUALS one or CONTAINS one. Equality is included because there is
// no legitimate reason to delete `/` or to re-permission the user's home directory from inside an
// agent session — unlike the work scope below, where "act on the project root" is ordinary work.
//
// home may be empty (a caller that does not know it); the OS is then asked. An EMPTY value is always
// skipped rather than contributing a root, because filepath.Clean("") is ".", which would make every
// relative target "inside" it and refuse all relative work.
func Roots(home string) []string {
	if strings.TrimSpace(home) == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = h
		}
	}
	var out []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		p = filepath.Clean(p)
		if p == "" || p == "." {
			return
		}
		for _, seen := range out {
			if seen == p {
				return
			}
		}
		out = append(out, p)
	}

	// THE FILESYSTEM ROOT FIRST: `rm -rf /` is refused by equality, and it is the one target that
	// contains everything, so it belongs at the head of the list for a reader.
	add(string(filepath.Separator))
	add(home)
	if strings.TrimSpace(home) != "" {
		data := strings.TrimSpace(os.Getenv("XDG_DATA_HOME"))
		if data == "" {
			data = filepath.Join(home, ".local", "share")
		}
		add(filepath.Join(data, "orchicon"))
		add(filepath.Join(home, ".orchicon"))
	}
	return out
}

// ScopeRoots returns the WORK SCOPE: the project the session operates in and each session-granted
// directory.
//
// These are refused only when a target CONTAINS one — a PROPER ancestor. Acting ON the scope root is
// ordinary work (`chmod -R 755 <project>`, `rm -rf <project>/dist`), and refusing equality here would
// break it; destroying the thing that CONTAINS the scope is the catastrophe this rule is for, because
// it takes the scope with it and no amount of consent makes that the operator's intent.
//
// A grant widens what may be WRITTEN. It must not widen what may be DESTROYED to include the
// directory that holds the project, which is why the granted dirs are scope roots and not merely
// allow-list entries.
func ScopeRoots(projectDir string, grants []string) []string {
	var out []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		p = filepath.Clean(p)
		if p == "" || p == "." {
			return
		}
		for _, seen := range out {
			if seen == p {
				return
			}
		}
		out = append(out, p)
	}
	add(projectDir)
	for _, g := range grants {
		add(g)
	}
	return out
}

// DestroyedBy reports the root that target would destroy, and "" when the target is safe.
//
//	machineRoots — refused on EQUALS or CONTAINS
//	scopeRoots   — refused on CONTAINS only (a proper ancestor)
//
// It returns the ROOT rather than a bool so a refusal can NAME what would have been destroyed: the
// operator's next move after reading one is to decide whether the rule is right, and a message that
// says only "blocked" cannot be checked.
func DestroyedBy(target string, machineRoots, scopeRoots []string) string {
	t := strings.TrimSpace(target)
	if t == "" {
		return ""
	}
	t = filepath.Clean(t)
	if t == "" || t == "." || t == ".." {
		return ""
	}
	// The prefix a descendant must carry. THE FILESYSTEM ROOT IS THE EDGE CASE: appending a separator
	// to "/" gives "//", which no path starts with, so `rm -rf /` would slip through a containment test
	// that is otherwise right. (The machine-root list catches it by equality as well; this keeps the
	// helper correct on its own, which is what a reader of it will assume.)
	prefix := t + string(filepath.Separator)
	if t == string(filepath.Separator) {
		prefix = string(filepath.Separator)
	}
	for _, r := range machineRoots {
		if r == "" {
			continue
		}
		// EQUALS, or CONTAINS. The separator matters: a bare prefix test would make /home2 look like
		// it contains /home — the same off-by-a-separator bug the grant store documents.
		if t == r || strings.HasPrefix(r, prefix) {
			return r
		}
	}
	for _, r := range scopeRoots {
		if r == "" {
			continue
		}
		// CONTAINS ONLY: `t == r` is the scope itself and is left to the consent chain.
		if strings.HasPrefix(r, prefix) {
			return r
		}
	}
	return ""
}

// Refusal words the refusal for a target that would destroy root, naming both paths. The medium is a
// tool error the MODEL reads, and a second reader is the operator deciding whether the rule is right,
// so it states what would have been destroyed and why no approval can override it.
func Refusal(target, root string) string {
	return "refused: " + target + " contains " + root + ", which this session works in or depends on — " +
		"deleting it would take that with it. This is never allowed: not by a session grant, not by " +
		"an accept entry, and not by FULLSEND. Choose a target inside the project instead."
}

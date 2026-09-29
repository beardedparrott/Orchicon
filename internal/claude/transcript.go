package claude

// transcript.go — whether claude holds a transcript for a session id.
//
// # Why this decides the launch flag, and why getting it wrong is silent
//
// A claude session is a JSONL file on disk, and the two launch flags are NOT
// interchangeable:
//
//	--session-id <id>   CREATE a session with this id. If one already exists the
//	                    CLI refuses: "Session ID <id> is already in use." — exit 1,
//	                    and NOTHING on stdout.
//	--resume <id>       CONTINUE an existing session. If none exists the CLI
//	                    fails with "No conversation found with session ID <id>".
//
// The operator hit the first: opening an existing conversation re-used its stored
// id with `--session-id`, so every turn after the first died with an empty stdout
// — the message appeared and then nothing, which reads as the session "not
// latching on".
//
// So the flag must follow the DISK, not a guess about which turn this is. A
// respawn after a child died is the interesting case: the session is not new, so
// it needs --resume even though a new process is starting.
//
// # The check is deliberately directory-agnostic
//
// VERIFIED against the real CLI: a session created in /tmp/resumeA resumes from
// /tmp/resumeB and still answers from its history, so claude resolves the
// transcript by ID across project directories. Globbing `projects/*/<id>.jsonl`
// is therefore correct — and necessary, because the host and the container do NOT
// have the same cwd (the worker's container path differs from the host's), while
// ~/.claude IS the same directory on both (the daemon passes HOME=$HostHome and
// bind-mounts it at an identical path).

import (
	"os"
	"path/filepath"
	"strings"
)

// ClaudeProjectsDir is where the CLI keeps per-project transcripts
// (~/.claude/projects/<encoded-cwd>/<session-id>.jsonl).
func ClaudeProjectsDir() string {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// claudeSessionHasTranscript reports whether the CLI has a transcript for sid.
//
// Any project directory counts (see the file header): the id is what resumes, not
// the path. An unreadable or absent projects dir reports false, which makes the
// caller treat the session as NEW — the safe direction, because `--session-id` on
// a session that does exist is the silent failure, while `--session-id` on one
// that does not is exactly right.
func claudeSessionHasTranscript(sid string) bool {
	sid = strings.TrimSpace(sid)
	if sid == "" {
		return false
	}
	dir := ClaudeProjectsDir()
	if dir == "" {
		return false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	name := sid + ".jsonl"
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if st, serr := os.Stat(filepath.Join(dir, e.Name(), name)); serr == nil && !st.IsDir() {
			return true
		}
	}
	return false
}

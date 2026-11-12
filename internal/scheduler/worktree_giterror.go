package scheduler

// worktree_giterror.go — WHAT GIT SAID, kept instead of thrown away.
//
// THE FAULT THIS FIXES. `runGit` already captures git's combined output, but every provisioning caller wrote
// `if _, err := runGit(...)` — discarding the captured bytes and propagating the bare `exit status 128`. The
// reconciler then flattened that to `git worktree add: exit status 128` and recorded it in the run's
// run_context. So an operator whose run failed on Windows saw:
//
//	{"worktree_error": "git worktree add: exit status 128"}
//
// Exit 128 is git's GENERAL FATAL code. Auth failure, a bad ref, a missing directory and a corrupt repository
// all produce it, so the recorded message named none of them — while the `fatal:` line that DID name it had
// been created by git and dropped on the floor. The diagnosis cost several rounds of investigation.
//
// TWO PARTS, both required: keep the output, and stop describing the failure as an exit status.

import "strings"

// GitError is a failed git invocation WITH the output that explains it.
type GitError struct {
	// Args is the argument list as passed, WITHOUT the `-C <dir>` prefix, so a call site can add the
	// directory it actually ran in (which is the thing that is wrong in a surprising number of Windows.
	Args []string
	// Dir is the working directory the command ran in, or ""
	// when it ran without one.
	Dir string
	// Output is git's combined output. Empty when git produced none (a permission error before it could
	// write, a killed process), which is itself diagnostic.
	Output string
	// Err is the underlying failure — usually an *exec.ExitError. Kept so errors.As/Is still reach the
	// cause when a caller needs the exit code.
	Err error
}

// Error renders the failure the way a diagnosis needs it: the COMMAND, the DIRECTORY, and what git
// SAID.
func (e *GitError) Error() string {
	if e == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("git ")
	b.WriteString(strings.Join(e.Args, " "))
	b.WriteString(" failed")
	if e.Dir != "" {
		b.WriteString(" in ")
		b.WriteString(e.Dir)
	}
	if fatal := e.FatalLine(); fatal != "" {
		b.WriteString(": ")
		b.WriteString(fatal)
	} else if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	if out := strings.TrimRight(e.Output, "\n"); out != "" && !strings.Contains(out, strings.TrimSpace(e.FatalLine())) {
		// Only append the dump when the summary did not already quote the whole story — the common case
		// is one fatal line, and repeating it wastes the reader's attention.
		b.WriteString("\n")
		b.WriteString(out)
	}
	return b.String()
}

// FatalLine returns git's own `fatal:` (or `error:`) line from Output, or "".
func (e *GitError) FatalLine() string {
	if e == nil {
		return ""
	}
	for _, line := range strings.Split(e.Output, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "fatal:") || strings.HasPrefix(t, "error:") {
			return t
		}
	}
	return ""
}

// Unwrap exposes the underlying error, so errors.Is(err, exec.ErrNotFound) and friends still work.
func (e *GitError) Unwrap() error { return e.Err }

// gitError builds a GitError from a failed run, preferring the output git actually produced.
func gitError(dir string, args []string, out string, err error) *GitError {
	return &GitError{Args: args, Dir: dir, Output: out, Err: err}
}

// worktreeProvisionError is the failure a run or step-run records when it could not create its
// working tree.
func worktreeProvisionError(gitErr error, strategy, branch, base, projectDir string) string {
	var b strings.Builder
	b.WriteString("could not create the run worktree")
	b.WriteString(": git strategy " + strategyOrUnknown(strategy))
	if branch != "" {
		b.WriteString(", branch " + branch)
	} else {
		b.WriteString(", detached (no branch)")
	}
	if base != "" {
		b.WriteString(", base ref " + base)
	} else {
		b.WriteString(", base ref HEAD")
	}
	b.WriteString(", in " + projectDir)
	if gitErr != nil {
		b.WriteString(" — ")
		b.WriteString(gitErr.Error())
	}
	return b.String()
}

func strategyOrUnknown(strategy string) string {
	if strategy == "" {
		return "(unset)"
	}
	return strategy
}

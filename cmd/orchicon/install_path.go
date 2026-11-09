package main

// install_path.go — PUT THE INSTALL DIR ON THE PATH, IN THE FILE A LOGIN SHELL ACTUALLY READS.

import (
	"bufio"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/beardedparrott/orchicon/internal/procwin"
)

// pathHint is WHAT HAPPENED when the install dir was put on the login shell's PATH.
type pathHint struct {
	// Dir is the install dir the hint is about.
	Dir string
	// OnPath is true when the dir is already exported in this process's PATH, so nothing was written.
	OnPath bool
	// Profile is the login profile consulted: written when Changed, or found to carry the entry already.
	Profile string
	// Changed is true when the entry was appended by this run.
	Changed bool
	// Problem is why the entry could not be placed. The summary falls back to advice when it is set, so a
	// host we cannot handle by hand is told how to do it rather than left with a silent no-op.
	Problem string
}

// ensureLoginProfilePATH puts installDir on the PATH for future LOGIN shells, idempotently.
func ensureLoginProfilePATH(installDir string) pathHint {
	out := pathHint{Dir: installDir}
	if strings.TrimSpace(installDir) == "" {
		out.Problem = "no install directory to add to PATH"
		return out
	}
	if dirOnPath(installDir) {
		out.OnPath = true
		return out
	}
	if runtime.GOOS == "windows" {
		// The PowerShell installer owns PATH on Windows (a registry value); the summary prints the
		// PowerShell equivalent, because "export PATH=..." is a syntax error there.
		return out
	}

	home, err := os.UserHomeDir()
	if err != nil {
		out.Problem = fmt.Sprintf("could not resolve the home directory: %v", err)
		return out
	}
	profile, shell, ok := loginProfileFor(resolveLoginShell(), home)
	if !ok {
		out.Problem = fmt.Sprintf("unrecognised shell %q — add %s to its profile by hand", shell, installDir)
		return out
	}
	out.Profile = profile

	existing, rerr := os.ReadFile(profile)
	if rerr != nil && !os.IsNotExist(rerr) {
		out.Problem = fmt.Sprintf("could not read %s: %v", profile, rerr)
		return out
	}
	if mentionsDir(string(existing), installDir) {
		return out // a previous install already did this
	}
	entry, eerr := pathExportLine(installDir)
	if eerr != nil {
		out.Problem = eerr.Error()
		return out
	}
	if err := os.MkdirAll(filepath.Dir(profile), 0o755); err != nil {
		out.Problem = fmt.Sprintf("could not create the profile's directory: %v", err)
		return out
	}
	body := string(existing)
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	body += "\n# Orchicon: so `orchicon` and `orch` resolve in a new terminal (added by the installer).\n" + entry + "\n"
	if err := os.WriteFile(profile, []byte(body), 0o644); err != nil {
		out.Problem = fmt.Sprintf("could not write %s: %v", profile, err)
		return out
	}
	out.Changed = true
	return out
}

// loginProfileFor returns the profile a NEW LOGIN shell reads, for the operator's shell.
func loginProfileFor(shell, home string) (profile, name string, ok bool) {
	name = strings.TrimPrefix(filepath.Base(strings.TrimSpace(shell)), "-") // login shells report "-zsh"
	switch name {
	case "zsh":
		return filepath.Join(home, ".zprofile"), name, true
	case "bash":
		for _, cand := range []string{".bash_profile", ".bash_login", ".profile"} {
			p := filepath.Join(home, cand)
			if _, err := os.Stat(p); err == nil {
				return p, name, true
			}
		}
		return filepath.Join(home, ".bash_profile"), name, true
	}
	return "", name, false
}

// mentionsDir reports whether a profile already puts dir on PATH, comparing COMPLETE path elements
// of a live (uncommented) line.
func mentionsDir(profile, dir string) bool {
	for _, line := range strings.Split(profile, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue // a commented-out export is not an export
		}
		// Split on everything a PATH assignment uses as a separator, then compare elements exactly.
		for _, tok := range strings.FieldsFunc(t, func(r rune) bool {
			return r == ':' || r == '"' || r == '\'' || r == ' ' || r == '=' || r == ';'
		}) {
			if tok == dir {
				return true
			}
		}
	}
	return false
}

// resolveLoginShell names the operator's login shell.
func resolveLoginShell() string {
	if s := strings.TrimSpace(os.Getenv("SHELL")); s != "" {
		return s
	}
	if runtime.GOOS == "darwin" {
		if home, err := os.UserHomeDir(); err == nil {
			// dscl reads the directory record for the current account without a password prompt.
			if out, cerr := procwin.Command("dscl", ".", "-read", home, "UserShell").Output(); cerr == nil {
				for _, line := range strings.Split(string(out), "\n") {
					if v, ok := strings.CutPrefix(strings.TrimSpace(line), "UserShell:"); ok {
						return strings.TrimSpace(v)
					}
				}
			}
		}
	}
	// Anything else POSIX: the passwd entry's last field.
	if u, err := user.Current(); err == nil {
		if f, ferr := os.Open("/etc/passwd"); ferr == nil {
			defer f.Close()
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				fields := strings.Split(sc.Text(), ":")
				if len(fields) >= 7 && fields[0] == u.Username {
					return strings.TrimSpace(fields[6])
				}
			}
		}
	}
	return ""
}

// pathExportLine renders the line to append.
func pathExportLine(dir string) (string, error) {
	if strings.ContainsAny(dir, "\n\r") {
		return "", fmt.Errorf("the install directory contains a newline, so it cannot go in a profile line")
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "`", "\\`")
	return `export PATH="` + r.Replace(dir) + `:$PATH"`, nil
}

// reportLauncherOnly finishes a `--install-launcher` run: it reports the launcher, and then puts
// the install dir on the login PATH.
func reportLauncherOnly(l orchLauncher, installDir string) error {
	if !l.Installed {
		return fmt.Errorf("the orch launcher could not be installed: %s", l.Problem)
	}
	fmt.Printf("orch launcher installed at %s (%s)\n", l.Path, l.Method)
	printPathHint(ensureLoginProfilePATH(installDir), installDir)
	return nil
}

// printPathHint renders the PATH outcome in the install summary.
func printPathHint(h pathHint, installDir string) {
	dir := h.Dir
	if dir == "" {
		dir = installDir
	}
	switch {
	case h.OnPath:
		// Already exported in the PATH this process ran with: nothing changes and nothing needs saying.
	case h.Changed:
		fmt.Printf("  Added %s to PATH in %s\n", dir, h.Profile)
		fmt.Printf("    Open a NEW terminal for `orchicon` and `orch` to resolve there — this one kept the PATH it started with.\n")
	case h.Problem == "" && h.Profile != "":
		fmt.Printf("  %s is already on your PATH via %s\n", dir, h.Profile)
	case runtime.GOOS == "windows":
		fmt.Printf("  %s is not on your PATH — add it to your user PATH (then open a NEW terminal):\n", dir)
		fmt.Printf("    [Environment]::SetEnvironmentVariable(\"Path\", \"$([Environment]::GetEnvironmentVariable('Path','User'));%s\", \"User\")\n", dir)
	default:
		fmt.Printf("  %s is not on your PATH — add it to your shell profile:\n", dir)
		fmt.Printf("    export PATH=\"%s:$PATH\"\n", dir)
		if h.Problem != "" {
			fmt.Printf("    (%s)\n", h.Problem)
		}
	}
}

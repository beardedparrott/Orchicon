package main

// install_flags.go — the flags the WINDOWS WIZARD drives `orchicon install` with.

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
)

// installFlags is everything the wizard can decide.
type installFlags struct {
	// Ports pins a port by its environment name (ORCHICON_CONTROL_PORT=8080), matching the
	// installer port table exactly — the same key the env route uses, so there is one spelling.
	Ports map[string]int
	// RuntimeImages is the runtime-image selection: "none", "all", or a comma list ("1,3").
	// Empty means "not decided" and leaves the existing prompt/record logic alone.
	RuntimeImages string
	// NonInteractive says "everything has already been decided; do not ask". The WINDOWS WIZARD
	// sets it, because it has just collected every answer on its own pages.
	NonInteractive bool
	// WantsConsole records --console: give this install its own console and detach onto it. Set so the flag is
	// visible to a caller, and CONSUMED here so it never reaches the instance parser (see the arm's note).
	WantsConsole bool
	// LaunchDesktop opens the desktop app once the control plane is confirmed healthy.
	LaunchDesktop bool
	// LauncherOnly retries the companion `orch` launcher and nothing else.
	LauncherOnly bool
}

// parseInstallFlags extracts the wizard's flags and returns the argv with them removed, so the
// instance parser downstream sees only what it understands.
func parseInstallFlags(args []string) (installFlags, []string, error) {
	out := installFlags{Ports: map[string]int{}}
	rest := make([]string, 0, len(args))

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--port" || arg == "-port":
			if i+1 >= len(args) {
				return out, nil, fmt.Errorf("--port needs NAME=PORT (e.g. --port ORCHICON_CONTROL_PORT=8080)")
			}
			i++
			env, value, ok := strings.Cut(args[i], "=")
			if !ok || strings.TrimSpace(env) == "" {
				return out, nil, fmt.Errorf("--port %q must be NAME=PORT", args[i])
			}
			if strings.TrimSpace(value) == "" {
				// AN EMPTY VALUE IS "NO PIN", not an error: the wizard's port box is blank whenever the operator
				// accepts the instance default, and failing there would break the ordinary case to catch a typo.
				continue
			}
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil || n < 1 || n > 65535 {
				return out, nil, fmt.Errorf("--port %s: %q is not a port (1..65535)", env, value)
			}
			out.Ports[strings.TrimSpace(env)] = n

		case strings.HasPrefix(arg, "--port=") || strings.HasPrefix(arg, "-port="):
			_, spec, _ := strings.Cut(arg, "=")
			env, value, ok := strings.Cut(spec, "=")
			if !ok || strings.TrimSpace(env) == "" {
				return out, nil, fmt.Errorf("--port=%q must be NAME=PORT", spec)
			}
			if strings.TrimSpace(value) == "" {
				continue
			}
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil || n < 1 || n > 65535 {
				return out, nil, fmt.Errorf("--port %s: %q is not a port (1..65535)", env, value)
			}
			out.Ports[strings.TrimSpace(env)] = n

		case arg == "--launch-desktop" || arg == "-launch-desktop":
			// A PLAIN SWITCH, not a value: "after a healthy install, open the app" is a yes/no, and a value
			// form invites the empty-argument hazard this parser already documents elsewhere.
			out.LaunchDesktop = true

		case arg == "--install-launcher" || arg == "-install-launcher":
			// CONSUMED, like --console and for the same reason: `rest` reaches parseInstanceTarget, which
			// rejects every flag it does not know, so leaving this in would exit with `unknown flag`.
			out.LauncherOnly = true

		case arg == "--console" || arg == "-console":
			// CONSUMED, NOT PASSED THROUGH.
			out.WantsConsole = true

		case arg == "--non-interactive" || arg == "-non-interactive":
			// THE WIZARD ANSWERED EVERYTHING, SO THERE IS NOTHING LEFT TO ASK.
			out.NonInteractive = true

		case arg == "--runtime-images" || arg == "-runtime-images":
			if i+1 >= len(args) {
				return out, nil, fmt.Errorf("--runtime-images needs a value: none, all, or a list like 1,3")
			}
			i++
			out.RuntimeImages = strings.TrimSpace(args[i])

		case strings.HasPrefix(arg, "--runtime-images=") || strings.HasPrefix(arg, "-runtime-images="):
			_, value, _ := strings.Cut(arg, "=")
			out.RuntimeImages = strings.TrimSpace(value)

		default:
			rest = append(rest, arg)
		}
	}
	return out, rest, nil
}

// hasNonInteractiveFlag peeks for --non-interactive WITHOUT consuming anything.
func hasConsoleFlag(args []string) bool {
	for _, a := range args {
		if a == "--console" || a == "-console" {
			return true
		}
	}
	return false
}

func hasNonInteractiveFlag(args []string) bool {
	for _, a := range args {
		if a == "--non-interactive" || a == "-non-interactive" {
			return true
		}
	}
	return false
}

// hasLauncherOnlyFlag peeks for --install-launcher WITHOUT consuming anything.
func hasLauncherOnlyFlag(args []string) bool {
	for _, a := range args {
		if a == "--install-launcher" || a == "-install-launcher" {
			return true
		}
	}
	return false
}

// exeSuffix returns the platform's executable suffix, so a message can name `orch.exe` on Windows and `orch`
// everywhere else instead of printing one and meaning the other.
func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

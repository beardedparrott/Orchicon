package main

// install_flags.go — the flags the WINDOWS WIZARD drives `orchicon install` with.
//
// WHY THESE EXIST. The MSI's custom action runs the installed binary directly (FileRef, no
// shell) and can pass ARGUMENTS but cannot set the child's ENVIRONMENT. Every answer the
// wizard collects therefore has to arrive on the command line, or the wizard's pages are
// decoration and the setup silently falls back to its defaults.
//
// They are deliberately the same values the environment already carries (ORCHICON_<X>_PORT,
// ORCHICON_INSTALL_RUNTIME_IMAGES), so the two routes cannot diverge: a flag simply pins what
// the env var would have pinned.

import (
	"fmt"
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
}

// parseInstallFlags extracts the wizard's flags and returns the argv with them removed, so the
// instance parser downstream sees only what it understands.
//
// DELIBERATELY STRICT. An unknown or malformed value is an ERROR rather than a silent default:
// a wizard page whose answer was dropped installs something the operator did not choose, and
// they have no way to tell. `--port` without a name=value pair names the flag in the error.
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
				// AN EMPTY VALUE IS "NO PIN", not an error: the wizard's port box is blank
				// whenever the operator accepts the instance default, and failing there would
				// break the ordinary case to catch a typo.
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

		case arg == "--non-interactive" || arg == "-non-interactive":
			// THE WIZARD ANSWERED EVERYTHING, SO THERE IS NOTHING LEFT TO ASK.
			//
			// Without this the setup opened a console and put the SAME questions again — the
			// instance, then each port — which is what "it still brought up the script and asked
			// me to fill in all of the same information" describes. The MSI already carried a
			// SKIPCONFIG property for this; nothing ever read it, and this flag is the thing the
			// custom action can actually pass.
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
//
// It exists because the decision it feeds — "should I relaunch into a console?" — has to be made
// BEFORE the full parse, and doing the full parse twice would mean two error paths for the same
// malformed flag. A peek is the smaller thing to get right.
func hasNonInteractiveFlag(args []string) bool {
	for _, a := range args {
		if a == "--non-interactive" || a == "-non-interactive" {
			return true
		}
	}
	return false
}

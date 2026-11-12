package verify

// declaration.go — THE PROJECT'S GATE DECLARATION, PARSED IN ONE PLACE.
//
// `.orchicon/verify` used to be a single line: a shell command, and nothing else. That was enough to RUN a gate
// and not enough to READ one, so the platform guessed the results out of the output — which is the defect
// results.go replaces. The file therefore grows named keys:
//
//	command = go test -json ./... | gotestsum --junitfile -
//	format  = junit
//
// AND IT STAYS BACKWARD COMPATIBLE. A file whose first non-comment line is a bare command still means that
// command, with format `exit_code`. Every project that already has one keeps working, and the operator opts in
// to per-check results by adding two lines.
//
// WHY A FILE AND NOT A SETTING. The same reason projectVerifyCommand documented: a per-project schema field for
// something most projects will not set is a schema field nobody maintains, and this one has to be readable by a
// person in the repository, versioned with the code it describes, and visible in a diff.

import (
	"fmt"
	"strings"
)

// Declaration is a project's gate: the command to run, how its results are shaped, and where they live.
type Declaration struct {
	// Command is the shell command, run with the project dir as cwd. Empty means the project declares no gate.
	Command string
	// Format is how the command reports per-check results.
	Format ResultFormat
	// Path is an optional FILE the command writes its report to, relative to the project dir.
	Path string
}

// Declared reports whether the project declares a gate at all.
func (d Declaration) Declared() bool { return strings.TrimSpace(d.Command) != "" }

// PerCheck reports whether this declaration can yield per-check results, which is what the differential needs.
func (d Declaration) PerCheck() bool { return d.Declared() && d.Format.ParsesResults() }

// keyValuePrefixes are the keys the file may carry. Anything else on the left of an '=' that is not one of these
// is treated as part of a BARE command, because a shell command can legitimately contain '='.
var keyValuePrefixes = []string{"command", "format", "path"}

// ParseDeclaration reads a `.orchicon/verify` file body.
func ParseDeclaration(body string) (Declaration, error) {
	var d Declaration
	var significant []string
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		significant = append(significant, trimmed)
	}
	if len(significant) == 0 {
		return d, nil
	}
	if !isKeyValue(significant[0]) {
		// BARE COMMAND (the original format). Everything significant is the command, as before.
		d.Command = significant[0]
		d.Format = FormatExitCode
		return d, nil
	}
	for _, line := range significant {
		key, value, ok := splitKeyValue(line)
		if !ok {
			return d, fmt.Errorf("line %q is not `key = value`; a declaration is either all keys or one bare "+
				"command", line)
		}
		switch key {
		case "command":
			d.Command = value
		case "format":
			f, err := ParseResultFormat(value)
			if err != nil {
				return d, err
			}
			d.Format = f
		case "path":
			d.Path = value
		default:
			return d, fmt.Errorf("unknown key %q in the gate declaration (want: command, format, path)", key)
		}
	}
	if !d.Declared() {
		return d, fmt.Errorf("the declaration has no `command`")
	}
	return d, nil
}

// isKeyValue reports whether a line is `knownkey = value`.
func isKeyValue(line string) bool {
	_, _, ok := splitKeyValue(line)
	return ok
}

// THE KEY MUST BE KNOWN, deliberately: `GOFLAGS=-count=1 go test ./...` is a legal bare command
// whose first token looks like a key.
func splitKeyValue(line string) (key, value string, ok bool) {
	i := strings.Index(line, "=")
	if i <= 0 {
		return "", "", false
	}
	key = strings.ToLower(strings.TrimSpace(line[:i]))
	for _, known := range keyValuePrefixes {
		if key == known {
			return key, strings.TrimSpace(line[i+1:]), true
		}
	}
	return "", "", false
}

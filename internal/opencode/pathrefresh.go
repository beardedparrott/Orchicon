package opencode

// pathrefresh.go — PARSING A PERSISTED WINDOWS PATH, in a file with NO build tag.
//
// WHY UNTAGGED. The registry READ is Windows-only (pathrefresh_windows.go), but the parsing is not:
// it is a pure function of (a string, an environment), and the rules it encodes are each a way a real
// registry value differs from a naive split. Keeping it behind a `windows` build tag would make it
// testable only on Windows — which in practice means never, since the gate runs on Linux. A table
// that only one OS can check is a table that will not be checked.
//
// WHAT IT IS FOR. The plane's PATH is a snapshot of the process that started it
// (internal/planecontrol builds the child environment from os.Environ()), so on Windows a plane
// started before opencode was installed keeps looking in the places the operator has since replaced.
// Re-reading the persisted PATH is what makes "I can run it from anywhere" true for the plane too,
// without a restart.

import "strings"

// SplitPersistedPath turns a persisted PATH value into directories, expanding the %VAR% references
// Windows stores in it and dropping any entry that cannot be a real directory.
//
// IT TAKES AN ENUMERABLE ENVIRONMENT, NOT A LOOKUP FUNCTION, and that is a correctness decision
// rather than a convenience. Windows variable names are CASE-INSENSITIVE while the registry and the
// process regularly disagree about spelling — a persisted Path says %SystemRoot% and the process
// exports "SYSTEMROOT" — so expansion has to compare names case-insensitively, and comparing is only
// possible if the whole environment can be WALKED. A `func(string) string` cannot be walked, and the
// first version of this function silently dropped every directory whose variable was spelled
// differently from the reference. (Measured: `%SystemRoot%\system32` expanded to `\system32`.)
//
// environ is the "K=V" form os.Environ returns.
func SplitPersistedPath(val string, environ []string) []string {
	if strings.TrimSpace(val) == "" {
		return nil
	}
	lookup := envFolder(environ)
	var out []string
	for _, raw := range strings.Split(val, ";") {
		dir := strings.TrimSpace(raw)
		if dir == "" {
			continue
		}
		expanded, ok := expandWindowsVars(dir, lookup)
		if !ok {
			// A REFERENCE THAT DID NOT EXPAND KILLS THE WHOLE ENTRY, rather than leaving its tail.
			// `%NOT_SET%\bin` would otherwise survive as `\bin` — a root-level directory that is
			// probed on every resolution for ever, and that makes the "not found" message claim a
			// search it could never have completed. Dropping the entry is the honest answer: we do
			// not know where that directory is, so we cannot look in it.
			continue
		}
		if expanded == "" {
			continue
		}
		out = append(out, expanded)
	}
	return out
}

// envFolder builds a case-insensitive name→value lookup over "K=V" entries.
//
// THE FIRST WINNER IS THE LAST ENTRY, matching how a process environment actually resolves duplicates
// on Windows: a later assignment replaces an earlier one.
func envFolder(environ []string) func(string) (string, bool) {
	exact := make(map[string]string, len(environ))
	lower := make(map[string]string, len(environ))
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		exact[k] = v
		lower[strings.ToLower(k)] = v
	}
	return func(name string) (string, bool) {
		if v, ok := exact[name]; ok {
			return v, true
		}
		v, ok := lower[strings.ToLower(name)]
		return v, ok
	}
}

// expandWindowsVars substitutes %NAME% references, reporting false when one of them names a variable
// that does not exist.
//
// A LONE OR UNCLOSED '%' IS ALSO A FAILURE. `%UNCLOSED` and `%` are malformed values; treating them
// as literal text would produce a directory name containing '%', which cannot exist on Windows and
// would be probed forever.
func expandWindowsVars(s string, lookup func(string) (string, bool)) (string, bool) {
	if !strings.Contains(s, "%") {
		return s, true
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '%' {
			b.WriteByte(s[i])
			i++
			continue
		}
		end := strings.IndexByte(s[i+1:], '%')
		if end < 0 {
			return "", false // unclosed reference
		}
		name := s[i+1 : i+1+end]
		if name == "" {
			return "", false // "%%" is not a reference to anything
		}
		v, ok := lookup(name)
		if !ok {
			return "", false // names a variable that is not set
		}
		b.WriteString(v)
		i += end + 2
	}
	return b.String(), true
}

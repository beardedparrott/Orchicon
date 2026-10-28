package adapter

// freshpath.go — PARSING A PERSISTED WINDOWS PATH, in a file with NO build tag.
//
// LIFTED FROM internal/opencode, where it was written for exactly this problem and then could only be used by
// one adapter. The registry READ is Windows-only (freshpath_windows.go), but the PARSING is not: it is a pure
// function of (a string, an environment), and each rule it encodes is a way a real registry value differs from
// a naive split. Keeping it behind a build tag would make it testable only on Windows — which in practice
// means never, since the gate runs on Linux. A table only one OS can check is a table that will not be
// checked.
//
// WHY IT MATTERS AT ALL: the plane's PATH is a snapshot of the process that started it, so on Windows a plane
// started before an adapter was installed keeps looking in the places the operator has since replaced.
// Re-reading the persisted PATH is what makes "I can run it from anywhere" true for the plane too.

import "strings"

// SplitPersistedPath turns a persisted PATH value into directories, expanding the %VAR% references Windows
// stores in it and dropping any entry that cannot be a real directory.
//
// IT TAKES AN ENUMERABLE ENVIRONMENT, NOT A LOOKUP FUNCTION, and that is a correctness decision rather than a
// convenience. Windows variable names are CASE-INSENSITIVE while the registry and the process regularly
// disagree about spelling — a persisted Path says %SystemRoot% and the process exports "SYSTEMROOT" — so
// expansion has to compare names case-insensitively, and comparing is only possible if the whole environment
// can be WALKED. A `func(string) string` cannot be walked, and the first version of this function silently
// dropped every directory whose variable was spelled differently from the reference. (Measured:
// `%SystemRoot%\system32` expanded to `\system32`.)
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
			// A REFERENCE THAT DID NOT EXPAND KILLS THE WHOLE ENTRY rather than leaving its tail.
			// `%NOT_SET%\bin` would otherwise survive as `\bin` — a root-level directory probed on every
			// resolution forever, making a "not found" message claim a search it could never complete.
			// Dropping the entry is the honest answer: we do not know where that directory is.
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
// THE FIRST WINNER IS THE LAST ENTRY, matching how a process environment resolves duplicates on Windows: a
// later assignment replaces an earlier one.
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

// expandWindowsVars substitutes %NAME% references. ok is false when the string holds a reference that cannot
// be resolved — an unclosed %, an empty %%, or a name that is not set — because a partially expanded path is
// worse than a dropped one (see SplitPersistedPath).
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

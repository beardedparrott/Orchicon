package adapter

// freshpath.go — PARSING A PERSISTED WINDOWS PATH, in a file with NO build tag.

import "strings"

// SplitPersistedPath turns a persisted PATH value into directories, expanding the %VAR% references
// Windows stores in it and dropping any entry that cannot be a real directory.
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

// expandWindowsVars substitutes %NAME% references.
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

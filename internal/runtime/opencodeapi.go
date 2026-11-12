package runtime

// opencodeapi.go — the opencode HTTP paths the DAEMON probes, and why they are tried in both forms.

// OpenCodeHealthPaths are the readiness paths a serve may expose, newest surface last.
func OpenCodeHealthPaths() []string {
	return []string{"/global/health", "/api/info"}
}

// OpenCodeSessionCreatePaths are the session-create paths a serve may expose, same ordering rule.
func OpenCodeSessionCreatePaths() []string {
	return []string{"/session", "/api/session"}
}

// opencodeAcceptableCreateStatus reports whether a session-create status means the machinery is up.
func opencodeAcceptableCreateStatus(code int) bool {
	return code >= 200 && code < 300
}

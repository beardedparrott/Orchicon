// Package orchicon is the native provider substrate: hand-written wire
// clients (no third-party SDKs), the provider registry, credential
// resolution, the vendored model catalog, and the model sourcing service.
//
// This is substrate only — no RPC, no UI. Consumers: the native adapter,
// the settings Providers tab (sibling task), the model pickers, and the
// context-management compaction triggers.
package orchicon

import (
	"bytes"
	"errors"
	"fmt"
)

// Sentinel errors surfaced by providers. Callers test with errors.Is.
var (
	// ErrAuthMissing means no credential could be resolved. The error text
	// names the exact secret name or env var expected (actionable failure).
	ErrAuthMissing = errors.New("provider credential missing")
	// ErrContextLength means the request exceeded the model's context window.
	ErrContextLength = errors.New("context length exceeded")
	// ErrRateLimited means the provider returned a rate-limit response that
	// exhausted retries.
	ErrRateLimited = errors.New("rate limited")
	// ErrUpgradeRequired is the Command Code documented 403 upgrade_required
	// shape (plan lacks Provider-route access — e.g. the Go plan). The
	// commandcode transport flip consumes it; callers may also see it when
	// the legacy route itself rejects.
	ErrUpgradeRequired = errors.New("plan upgrade required")
	// ErrProviderUnavailable means the provider endpoint could not be
	// reached or failed transiently beyond retries.
	ErrProviderUnavailable = errors.New("provider unavailable")
)

// StatusError is a non-2xx provider HTTP response. Retryable classification
// lives in retry.go; the status is preserved for callers and tests.
type StatusError struct {
	StatusCode int
	Status     string
	Body       string // truncated response body (bounded)
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("provider status %d %s: %s", e.StatusCode, e.Status, e.Body)
}

// httpStatusError builds a *StatusError with a bounded body.
func httpStatusError(statusCode int, status string, body []byte) *StatusError {
	const maxBody = 2048
	b := body
	if len(b) > maxBody {
		b = b[:maxBody]
	}
	return &StatusError{StatusCode: statusCode, Status: status, Body: string(b)}
}

// isUpgradeRequired403 reports whether the response is the documented
// Command Code 403 upgrade_required shape (any JSON body carrying
// "upgrade_required", or a bare 403 — plans without Provider-route access
// return exactly that status on /provider/v1/*).
func isUpgradeRequired403(statusCode int, body []byte) bool {
	if statusCode != 403 {
		return false
	}
	if len(body) == 0 {
		return true
	}
	return containsFold(body, []byte("upgrade_required"))
}

// containsFold is an ASCII-insensitive substring check.
func containsFold(haystack, needle []byte) bool {
	return len(needle) == 0 || bytes.Contains(bytes.ToLower(haystack), bytes.ToLower(needle))
}

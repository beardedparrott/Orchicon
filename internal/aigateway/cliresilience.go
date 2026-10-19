package aigateway

// cliresilience.go — ONE retry policy for every opencode CLI call the gateway makes.
//
// THE OPERATOR: "resilience across the board, not just on one provider or one model type." The gateway's own
// surface is opencode CLI discovery, and it was ALREADY asymmetric: the model discoverer retries a transient
// empty result (models.go, emptyRetryAttempts), while the MCP discoverer — the same CLI, the same managed
// service, the same eviction window — retried NOTHING. A blip that the model list absorbed as a retry would
// surface on the MCP list as a failure, and if there was no cache to fall back on it became an error the
// operator read as "MCP is broken".
//
// That asymmetry is the thing being removed, not one of its symptoms. This file holds the classification and
// the loop; the discoverers supply only what is specific to them.
//
// WHAT IS RETRYABLE HERE, and why it is narrower than the provider layer:
//
//	TRANSIENT   a timeout, a connection failure, a CLI that exited non-zero with no diagnosis (the managed
//	            service evicting/rebooting), or an EMPTY result where empty is a known race rather than an
//	            answer. Worth another attempt.
//	PERMANENT   the CLI ran and SAID what was wrong (unparseable output, an unknown flag, a config error).
//	            Retrying re-runs the same command to collect the same words, so it only delays a diagnosis.
//
// The second half is not decoration: retrying a deterministic failure is how a clear error becomes a slow,
// vague one. `fetchModelsOnce` already draws this line for the model list ("A 'printed something unparseable'
// failure is deliberately NOT this"), and this file generalises the line rather than re-deciding it.

import (
	"context"
	"errors"
	"strings"
	"time"
)

// cliRetryAttempts and cliRetryDelay bound a retry of a TRANSIENT CLI failure. The window matches the model
// discoverer's (five attempts over ~4s), because the phenomenon is the same one: opencode's managed service
// evicts a location and lazily re-boots it, and a call landing in that window fails or comes back empty.
const (
	cliRetryAttempts = 5
	cliRetryDelay    = 900 * time.Millisecond
)

// retryableCLIError reports whether a CLI failure is worth another attempt.
//
// It is CONSERVATIVE in the same direction as the provider layer: it retries only what it can positively
// identify as transient, and treats everything else as deterministic. A false "permanent" costs a retry the
// operator never sees; a false "transient" turns a clear error into a slow one and buries the diagnosis.
func retryableCLIError(err error) bool {
	if err == nil {
		return false
	}
	// THE DOCUMENTED RACE: the service is mid-boot and reports an empty catalog rather than failing.
	if errors.Is(err, errNoModels) {
		return true
	}
	// A CANCELLED CONTEXT IS THE CALLER GIVING UP, never the CLI misbehaving. Retrying would ignore an
	// instruction to stop.
	if errors.Is(err, context.Canceled) {
		return false
	}
	// A CLI that HUNG. The timeout is what surfaced it and the next attempt may find the service up.
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	// A STRUCTURED CLI FAILURE: the wrapper recorded whether the CLI explained itself, so the decision is a
	// FACT rather than a reading of the prose. This replaced a phrase/length heuristic that mis-classified the
	// case a retry exists for — see cliError's note.
	var ce *cliError
	if errors.As(err, &ce) {
		return ce.detail == ""
	}

	// A PARSE OR CONFIG FAILURE from our own code is deterministic: the output was read and could not be
	// understood, so another attempt reads the same bytes.
	//
	// The error TYPE is the right signal here (unlike for a CLI exit, where the type says nothing about why),
	// and the decision is made on it BEFORE any text matching.
	msg := err.Error()
	if cliParseFailure(msg) {
		return false
	}
	// A start failure names the binary and is deterministic: the next attempt meets the same filesystem.
	low := strings.ToLower(msg)
	for _, deterministic := range []string{
		"executable file not found", "no such file or directory", "permission denied",
		"invalid argument", "cannot run",
	} {
		if strings.Contains(low, deterministic) {
			return false
		}
	}
	// What remains is a transport-level or unexplained failure: worth one more attempt.
	return true
}

// cliParseFailure reports whether an error is our own code failing to UNDERSTAND the CLI's output, which is
// deterministic whatever produced it.
//
// These markers are the discoverers' own wrapping of a read-and-could-not-parse outcome, and they are checked
// because such a failure is not a *cliError at all — it is produced after a SUCCESSFUL invocation.
func cliParseFailure(msg string) bool {
	low := strings.ToLower(msg)
	for _, marker := range []string{
		"unmarshal", "unexpected end of json", "invalid character",
		"parse opencode config", "scan model output",
		// The unparseable-output case, in the model discoverer's own words.
		"no models could be read", "output format is not one this version understands",
	} {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}

// withCLIRetries runs fn, retrying a TRANSIENT failure and returning a deterministic one immediately.
//
// emptyIsTransient tells the loop how to treat a nil error with an empty payload: for the model catalog an
// empty list is a known race (errNoModels), while for a config read an empty result means something else.
// The caller states which it is rather than this loop guessing, because the same shape means different things
// on different commands.
func withCLIRetries(ctx context.Context, log interface {
	Info(string, ...any)
}, what string, emptyIsTransient bool, fn func() (int, error)) (int, error) {
	var lastErr error
	for attempt := 1; attempt <= cliRetryAttempts; attempt++ {
		n, err := fn()
		if err == nil && (n > 0 || !emptyIsTransient) {
			if attempt > 1 && log != nil {
				log.Info("opencode CLI call succeeded on retry",
					"what", what, "attempt", attempt,
					"note", "the managed service evicts and lazily re-boots a location; a call landing in "+
						"that window fails or comes back empty")
			}
			return n, nil
		}
		if err == nil && emptyIsTransient {
			// An empty result that is a race, expressed as the same error the model discoverer uses so the
			// retry decision has one shape to reason about.
			err = errNoModels
		}
		lastErr = err
		if !retryableCLIError(err) {
			return n, err
		}
		if attempt < cliRetryAttempts {
			select {
			case <-time.After(cliRetryDelay):
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		}
	}
	return 0, lastErr
}
// withCLIRetries runs fn, retrying a TRANSIENT failure and returning a deterministic one immediately.
//
// emptyIsTransient tells the loop how to treat a nil error with an empty payload: for the model catalog an
// empty list is a known race (errNoModels), while for a config read an empty result means something else.
// The caller states which it is rather than this loop guessing, because the same shape means different things
// on different commands.

package runtimeimage

import (
	"regexp"
	"strings"
)

// BuildFailure is the structured classification of a failed docker build.
type BuildFailure struct {
	FailedStep string
	Reason     string
	Hint       string
	Category   string // engine_mismatch | apt_dpkg | network | invalid_tag | oom | dockerfile | stream | unknown
	LogTail    string // last 60 lines, capped
}

const logTailLines = 60
const logTailMaxBytes = 8 * 1024

var (
	reStepClassic  = regexp.MustCompile(`(?m)^Step\s+\d+/\d+\s*:\s*(.+)$`)
	reBuildKitStep = regexp.MustCompile(`(?m)ERROR\s+\[([^\]]+)\]`)
	reExecutorFail = regexp.MustCompile(`executor failed running \[([^\]]+)\]`)
)

// ClassifyBuildLog parses a docker build log into a structured failure.
// exitErr is the daemon/client error string (e.g. "exit status 1").
func ClassifyBuildLog(log string, exitCode int, exitErr string) BuildFailure {
	f := BuildFailure{Category: "unknown"}

	// Extract failing step: last match wins across common formats.
	var step string
	for _, m := range reStepClassic.FindAllStringSubmatch(log, -1) {
		v := strings.TrimSpace(m[1])
		if v != "" {
			step = v
		}
	}
	if step == "" {
		for _, m := range reBuildKitStep.FindAllStringSubmatch(log, -1) {
			v := strings.TrimSpace(m[1])
			if v != "" {
				step = v
			}
		}
	}
	if step == "" {
		for _, m := range reExecutorFail.FindAllStringSubmatch(log, -1) {
			v := strings.TrimSpace(m[1])
			if v != "" {
				step = v
			}
		}
	}
	f.FailedStep = step

	// Log tail
	f.LogTail = buildLogTail(log)

	lower := strings.ToLower(log)
	lowerErr := strings.ToLower(exitErr)

	// Detect categories in priority order.
	switch {
	case containsAny(lower, []string{"ebadengine", "engine mismatch", "unsupported engine"}) ||
		(strings.Contains(lower, "playwright") && strings.Contains(lower, "requires") && strings.Contains(lower, "node")):
		f.Category = "engine_mismatch"
		f.Reason = "Engine mismatch: Playwright requires Node >= 20 but the base image ships Node 18"
		if line := findLine(log, "EBADENGINE", "engine"); line != "" {
			f.Reason = line
		}
		f.Hint = "Playwright requires Node >= 20 but the image ships Node 18\u2014 install Node 20+ in the Dockerfile before installing Playwright (e.g. via NodeSource or `mise install node@20` / `toolchains: [\"node 20\"]`)."
	case containsAny(lower, []string{"unable to locate package", "dpkg returned an error", "dpkg: error", "e: unable to"}):
		f.Category = "apt_dpkg"
		f.Reason = extractReasonLine(log, []string{"Unable to locate package", "dpkg", "E:"})
		if f.Reason == "" {
			f.Reason = "APT/dpkg install failed"
		}
		f.Hint = "APT package not found or dpkg failed\u2014 check package names and run `apt-get update` before installing."
	case containsAny(lower, []string{"dial tcp", "tls handshake timeout", "could not resolve host", "temporary failure in name resolution", "429 too many requests", "connection timed out", "connection reset", "network is unreachable"}) ||
		containsAny(lowerErr, []string{"dial tcp", "timeout", "could not resolve"}):
		f.Category = "network"
		f.Reason = extractReasonLine(log, []string{"dial tcp", "TLS handshake", "Could not resolve", "429", "timeout"})
		if f.Reason == "" {
			f.Reason = "Network timeout or DNS failure during build"
		}
		f.Hint = "Network or registry unavailable\u2014 retry the build; check connectivity and registry status."
	case strings.Contains(lower, "invalid reference format") || strings.Contains(lower, "invalid tag"):
		f.Category = "invalid_tag"
		f.Reason = extractReasonLine(log, []string{"invalid reference", "invalid tag"})
		if f.Reason == "" {
			f.Reason = "Invalid image tag format"
		}
		f.Hint = "Tag is not a valid Docker reference\u2014 use lowercase, e.g. `my-image:latest`."
	case containsAny(lower, []string{"killed", "signal 9", "out of memory", "ex137", "oom", "cannot allocate memory"}):
		f.Category = "oom"
		f.Reason = extractReasonLine(log, []string{"Killed", "out of memory", "OOM", "signal 9"})
		if f.Reason == "" {
			f.Reason = "Build killed\u2014 likely out of memory"
		}
		f.Hint = "Build was killed (OOM)\u2014 increase memory or reduce build layers."
	case containsAny(lower, []string{"dockerfile: syntax", "dockerfile parse error", "unknown instruction", "failed to parse dockerfile"}):
		f.Category = "dockerfile"
		f.Reason = extractReasonLine(log, []string{"syntax", "Dockerfile", "unknown instruction"})
		if f.Reason == "" {
			f.Reason = "Dockerfile syntax error"
		}
		f.Hint = "Dockerfile syntax error\u2014 check FROM/RUN/COPY instructions."
	case strings.Contains(lowerErr, "input stream") || strings.Contains(lower, "error in input stream"):
		f.Category = "stream"
		f.Reason = "Build log stream disconnected"
		f.Hint = "Log stream dropped\u2014 build may still be running; check status or cancel and retry."
	default:
		if line := lastErrorLine(log); line != "" {
			f.Reason = line
		} else if exitErr != "" && !isGenericExit(exitErr) {
			f.Reason = exitErr
		} else if f.LogTail != "" {
			f.Reason = "Build failed\u2014 see log tail"
		} else {
			f.Reason = "Build failed: " + exitErr
			if f.Reason == "Build failed: " {
				f.Reason = "Build failed"
			}
		}
	}

	if isGenericExit(f.Reason) && f.LogTail != "" {
		f.Reason = "Build failed\u2014 see log tail"
	}
	return f
}

func buildLogTail(log string) string {
	if log == "" {
		return ""
	}
	lines := strings.Split(log, "\n")
	if len(lines) > logTailLines {
		lines = lines[len(lines)-logTailLines:]
	}
	tail := strings.Join(lines, "\n")
	if len(tail) > logTailMaxBytes {
		tail = tail[len(tail)-logTailMaxBytes:]
		if idx := strings.Index(tail, "\n"); idx >= 0 {
			tail = tail[idx+1:]
		}
	}
	return strings.Trim(tail, "\n")
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, strings.ToLower(sub)) {
			return true
		}
	}
	return false
}

func findLine(log string, keywords ...string) string {
	for _, line := range strings.Split(log, "\n") {
		lower := strings.ToLower(line)
		for _, kw := range keywords {
			if strings.Contains(lower, strings.ToLower(kw)) {
				trim := strings.TrimSpace(line)
				if trim != "" {
					return trim
				}
			}
		}
	}
	return ""
}

func extractReasonLine(log string, keywords []string) string {
	return findLine(log, keywords...)
}

func lastErrorLine(log string) string {
	lines := strings.Split(log, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		if strings.Contains(lower, "error") || strings.Contains(lower, "failed") || strings.Contains(lower, "fatal") {
			return line
		}
	}
	return ""
}

func isGenericExit(s string) bool {
	t := strings.TrimSpace(strings.ToLower(s))
	return t == "exit status 1" || t == "runtime image build: exit status 1" || t == "docker build failed" || t == "exit status 1: see log tail"
}

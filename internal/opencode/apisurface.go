package opencode

// apisurface.go — WHICH HTTP API an opencode serve exposes, decided by its version.
//
// WHY THIS EXISTS. opencode 2.x replaced its HTTP surface. V2_HTTP_API_AUDIT.md in the opencode repo
// ("Source: packages/protocol/openapi.json", 139 endpoints) shows every session endpoint moving under
// /api with renamed operations, and an operation-ID scheme (server.info, session.create, …) that the
// 1.x surface never had. Measured against the two versions this project actually runs:
//
//	1.18.34 (host)   GET /global/health, POST /session/{id}/prompt_async, /session/{id}/summarize, …
//	2.0.26 (WSL)     GET /api/info,      POST /api/session/{id}/prompt,        /api/session/{id}/compact, …
//
// A plane that speaks only one of these fails on the other, and it fails in the least helpful way:
// the session transport is what carries every worker turn, so the symptom is a worker that never
// starts rather than an error that names an endpoint.
//
// THE SHAPES ARE DATA, AND THAT IS THE POINT. The paths below are one table each, so supporting a
// third surface — or correcting one of these — is an edit in this file rather than a hunt through the
// client. The client asks the shape for a path; it never builds one.
//
// WHAT IS VERIFIED AND WHAT IS NOT. The v2 PATHS are taken from the audit's own endpoint table and
// each one is quoted in the build below. The v2 REQUEST BODIES are NOT verified: the audit documents
// two renames (permission reply `reply`→`decision`, session command `command`→`name`) and says
// nothing about the create/prompt/compact payloads, this project has no opencode 2.x on the machine
// that wrote this, and the audit explicitly omits request schemas. So the bodies are assumed
// compatible and the two documented renames are encoded; a live 2.x smoke test is REQUIRED before
// this can be called verified, and the fields to check are called out on apiShape.

import (
	"context"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// apiShape describes one version of the serve's HTTP API.
//
// Every field is a PATH FRAGMENT, never a full URL: the client owns its base URL and the shape owns
// only the routes, so a shape can be table-tested without a server.
type apiShape struct {
	// name is "v1" or "v2" — for logs, errors and test failures, never for logic.
	name string
	// prefix is prepended to every session route: "" on v1, "/api" on v2.
	prefix string
	// healthPath is the readiness probe. It is NOT prefix+something on either version: v1 uses
	// /global/health and v2 replaced it with /api/info (the audit merged and renamed the old health
	// and server endpoints into server.info).
	healthPath string
	// The suffixes that differ between versions, kept as data so a correction is one line.
	promptSuffix  string // v1 "prompt_async" | v2 "prompt"
	abortSuffix   string // v1 "abort"        | v2 "interrupt"
	compactSuffix string // v1 "summarize"    | v2 "compact"
	// approvalStyle selects the permission-reply ROUTE SHAPE, which is not a suffix swap: v1 nests a
	// collection (/permissions/{id}) and v2 addresses the reply as a sub-resource
	// (/permission/{id}/reply). Modelled as a flag rather than a builder func so the shape stays
	// comparable and table-testable.
	permissionReplyNested bool
	// permissionReplyField is the JSON key carrying the decision: v1 "response", v2 "decision"
	// (audit: "Renamed request field from `reply` to `decision`").
	//
	// ⚠ UNVERIFIED beyond the audit's note — see the file comment.
	permissionReplyField string

	// responseEnvelope is the key v2 wraps SESSION responses in, or "" when responses are bare.
	//
	// MEASURED against opencode 2.0.26 — and the envelope is PER-ENDPOINT, not universal, which is why
	// this is a key name and the decoder tolerates its absence:
	//
	//	POST /api/session          -> {"data":{"id":"ses_…","projectID":…,"title":…}}
	//	GET  /api/session/{id}     -> {"data":{"id":"ses_…",…}}
	//	GET  /api/info             -> {"version":"2.0.26","pid":…,"urls":[…]}      <- NO envelope
	//
	// Decoding the envelope blindly would therefore break the endpoints that do not use it, so the
	// decoder unwraps WHEN THE KEY IS PRESENT and passes the body through when it is not.
	responseEnvelope string
	// modelsArgs is the argv used to LIST MODELS on this surface.
	//
	// v1: `models --verbose` — the metadata form, the only one that carries cost and limits.
	// v2: `models` — NO FLAG.
	//
	// --standalone WAS TRIED ON v2 AND IT IS WRONG, which is worth recording because it looks right
	// on paper. The v2 handler resolves a server before listing (services/server-connection.ts):
	//
	//	if (args.standalone || (yield* ServiceConfig.read()).disabled === true) {
	//	  return { endpoint: yield* Standalone.start() }     // a private, ephemeral server
	//	}
	//	return { endpoint: yield* resolveManaged(options) }  // Service.ensure — the MANAGED service
	//
	// so --standalone avoids starting the managed service, which was the reported HANG. But MEASURED
	// against opencode 2.0.26 on the operator's own machine, with the same binary:
	//
	//	opencode models              -> 70 ms, 548 lines, 18,648 bytes, all 11 providers
	//	opencode models --standalone -> 0 bytes
	//
	// The ephemeral server serves an EMPTY model catalog (it is fresh, it did not fetch models.dev,
	// and `--refresh` — which would force that — is rejected by 2.0.26 as an unrecognized flag). So
	// --standalone trades the hang for an empty list, which is a worse failure: the picker silently
	// has nothing to offer. The managed service is the only path with models, so it is the one we
	// use, and the cold-start cost of reaching it is handled by the caller's timeout rather than by
	// avoiding it.
	modelsArgs []string
}

// shapeV1 is the surface the adapter was written against, and the DEFAULT. It is default because it
// is what every existing install in the field runs; a detection failure must not silently switch an
// operator to a surface their serve does not implement.
var shapeV1 = apiShape{
	name:                  "v1",
	prefix:                "",
	healthPath:            "/global/health",
	promptSuffix:          "prompt_async",
	abortSuffix:           "abort",
	compactSuffix:         "summarize",
	permissionReplyNested: true,
	permissionReplyField:  "response",
	responseEnvelope:      "",
	modelsArgs:            []string{"models", "--verbose"},
}

// shapeV2 is opencode 2.x. Every path here is quoted from V2_HTTP_API_AUDIT.md's endpoint table:
//
//	| 001–002 | GET  | /api/info                                  | server.info   | Keep |
//	| 047     | GET  | /api/session                               | session.list  | Keep |
//	| 048     | POST | /api/session                               | session.create| Keep |
//	| 051     | GET  | /api/session/{sessionID}                   | session.get   | Keep |
//	| 059     | POST | /api/session/{sessionID}/prompt            | session.prompt| Keep |
//	| 064     | POST | /api/session/{sessionID}/compact           | session.compact| Keep |
//	| 067     | POST | /api/session/{sessionID}/interrupt         | session.interrupt | Change |
//	| 100     | POST | /api/session/{sessionID}/permission/{requestID}/reply | session.permission.reply | Change |
//	| 140     | GET  | /api/event                                 | event.subscribe | Keep |
var shapeV2 = apiShape{
	name:                  "v2",
	prefix:                "/api",
	healthPath:            "/api/info",
	promptSuffix:          "prompt",
	abortSuffix:           "interrupt",
	compactSuffix:         "compact",
	permissionReplyNested: false,
	permissionReplyField:  "decision",
	responseEnvelope:      "data",
	modelsArgs:            []string{"models"},
}

// Path builders. The client calls these and never concatenates a route itself — that is what keeps a
// version difference from leaking into call sites.

func (s apiShape) sessionsPath() string { return s.prefix + "/session" }

func (s apiShape) sessionPath(escapedID string) string { return s.prefix + "/session/" + escapedID }

func (s apiShape) promptPath(sessionID string) string {
	return s.prefix + "/session/" + sessionID + "/" + s.promptSuffix
}

func (s apiShape) abortPath(sessionID string) string {
	return s.prefix + "/session/" + sessionID + "/" + s.abortSuffix
}

func (s apiShape) compactPath(sessionID string) string {
	return s.prefix + "/session/" + sessionID + "/" + s.compactSuffix
}

// permissionReplyPath is the one route whose SHAPE changed rather than its name: v1 nests the reply
// in a permissions collection, v2 addresses it as a sub-resource of a singular permission.
func (s apiShape) permissionReplyPath(sessionID, permissionID string) string {
	if s.permissionReplyNested {
		return s.prefix + "/session/" + sessionID + "/permissions/" + permissionID
	}
	return s.prefix + "/session/" + sessionID + "/permission/" + permissionID + "/reply"
}

func (s apiShape) eventPath() string { return s.prefix + "/event" }

// modelsFallbackArgs is the plainest invocation, used when the surface's own form is rejected. It
// carries no flag at all, so it is the one thing every version must accept.
func modelsFallbackArgs() []string { return []string{"models"} }

// THE MAJOR IS THE ONLY THING THAT DECIDES. The audit is titled "V2", the two command trees agree
// that the break is at 2.0, and reading a minor or patch would invent precision this project does not
// have — there is no changelog entry that pins the switch to a patch release.
//
// An UNPARSEABLE version returns v1 and false rather than guessing: v1 is what the installed base
// runs, so a version we cannot read must leave behaviour exactly as it was. The bool is what lets the
// caller say so out loud instead of silently assuming.
func apiShapeForVersion(version string) (apiShape, bool) {
	v := strings.TrimSpace(version)
	v = strings.TrimPrefix(v, "v")
	if v == "" {
		return shapeV1, false
	}
	majorStr, _, _ := strings.Cut(v, ".")
	major, err := strconv.Atoi(strings.TrimSpace(majorStr))
	if err != nil {
		return shapeV1, false
	}
	if major >= 2 {
		return shapeV2, true
	}
	return shapeV1, true
}

// detectAPIShape asks the binary which surface it serves.
//
// It runs `opencode --version` — a subprocess, so it is called ONCE per long-lived client rather than
// per request. A failure is not fatal: the caller keeps v1 and should log the fact, because a client
// that silently picked a surface it could not confirm would produce the endpoint-shaped mystery this
// file exists to remove.
func detectAPIShape(ctx context.Context, binary string) (shape apiShape, version string, ok bool) {
	if strings.TrimSpace(binary) == "" {
		return shapeV1, "", false
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	out, err := exec.CommandContext(cctx, binary, "--version").Output()
	if err != nil {
		return shapeV1, "", false
	}
	version = strings.TrimSpace(string(out))
	// `--version` may print a banner line before or after the number; find the first token that
	// parses as a version rather than trusting the whole output.
	for _, field := range strings.Fields(version) {
		if shape, parsed := apiShapeForVersion(field); parsed {
			return shape, field, true
		}
	}
	return shapeV1, version, false
}

// ModelsArgs returns the argv for LISTING MODELS with the opencode binary at path, in the form that
// binary's API surface requires.
//
// EXPORTED BECAUSE THE RULE MUST NOT BE RE-DERIVED. The discoverer lives in another package and needs
// the argv, and the version rule behind it lives here — v2 needs `--standalone` or the command starts
// a background daemon (see apiShape.modelsArgs). A second copy of that rule in the caller is how the
// two drift, and the symptom would be a hang that comes back.
func ModelsArgs(ctx context.Context, log *slog.Logger, binary string) []string {
	return shapeForBinary(ctx, log, binary).modelsArgs
}

type shapeDetection struct {
	shape   apiShape
	version string
	ok      bool
}

var (
	shapeCacheMu sync.Mutex
	shapeCache   = map[string]shapeDetection{}
)

// shapeForBinary returns the API surface served by the opencode binary at path, detecting it once.
//
// THE CACHE IS PROCESS-LIFETIME, which is sound HERE for a specific reason rather than by assumption:
// replacing the binary is exactly what makes the installer RESTART the host plane — startHostPlane
// compares /proc/<pid>/exe against the installed path and restarts a plane whose binary has drifted —
// so a long-lived process is not left holding a verdict about a binary that was swapped underneath it.
//
// A FAILED DETECTION IS LOGGED, NOT SWALLOWED. Silently assuming v1 is the right DEFAULT (see
// shapeV1) but the wrong SILENCE: on a 2.x install every session call would then fail against
// endpoints that do not exist, and nothing would say why. The warning names the version output so an
// operator can see what was unparseable.
func shapeForBinary(ctx context.Context, log *slog.Logger, binary string) apiShape {
	if strings.TrimSpace(binary) == "" {
		return shapeV1
	}
	shapeCacheMu.Lock()
	cached, hit := shapeCache[binary]
	shapeCacheMu.Unlock()
	if hit {
		return cached.shape
	}

	shape, version, ok := detectAPIShape(ctx, binary)
	if log != nil {
		if ok {
			log.Info("opencode API surface detected", "api", shape.name, "opencode_version", version, "binary", binary)
		} else {
			log.Warn("could not determine the opencode API surface — assuming v1",
				"opencode_version_output", version, "binary", binary,
				"note", "opencode 2.x serves a different HTTP API (endpoints under /api); if this binary is 2.x, set it aside and report this line, because sessions will fail against endpoints that do not exist")
		}
	}
	if ok {
		shapeCacheMu.Lock()
		shapeCache[binary] = shapeDetection{shape: shape, version: version, ok: ok}
		shapeCacheMu.Unlock()
	}
	return shape
}

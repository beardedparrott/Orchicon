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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/procwin"
)

// apiShape describes one version of the serve's HTTP API.
type apiShape struct {
	// name is "v1" or "v2" — for logs, errors and test failures, never for logic.
	name string
	// This is the property that decides two things, and getting it wrong is silently harmful rather
	// than loud.
	codeMode bool
	// prefix is prepended to every session route: "" on v1, "/api" on v2.
	prefix string
	// healthPath is the readiness probe.
	healthPath string
	// The suffixes that differ between versions, kept as data so a correction is one line.
	promptSuffix  string // v1 "prompt_async" | v2 "prompt"
	abortSuffix   string // v1 "abort"        | v2 "interrupt"
	compactSuffix string // v1 "summarize"    | v2 "compact"
	// approvalStyle selects the permission-reply ROUTE SHAPE, which is not a suffix swap: v1 nests a
	// collection (/permissions/{id}) and v2 addresses the reply as a sub-resource.
	permissionReplyNested bool
	// permissionReplyField is the JSON key carrying the decision: v1 "response", v2 "decision" (audit:
	// "Renamed request field from `reply` to `decision`").
	permissionReplyField string

	// Overriding ONLY the prompt route keeps the blast radius at the one call being changed.
	promptPathOverride func(sessionID string) string
	// promptStyle selects how a message is expressed, and the two are NOT interchangeable.
	//
	// v1: {"parts":[{"type":"text","text":…}], "system":…, "model":{"providerID":…,"modelID":…}}
	// v2: {"text":…, "files":[{"uri":…}]}          — text is REQUIRED and the other keys do not exist
	//
	// MEASURED against 2.0.26, from the OpenAPI spec the v2 audit names as its source
	// (packages/protocol/openapi.json, POST /api/session/{id}/prompt):
	//
	//	{"text": string, "files": [FileAttachment], "agents": […], "skills": […],
	//	 "metadata": {}, "delivery": "steer"|"queue", "resume": bool}
	//	required: ["text"], additionalProperties: false
	//
	// There is NO `system`, NO `model` and NO `parts` on v2 — so a v1 body is rejected with
	// `Missing key at ["text"]` (the operator's error, reproduced) and, worse, the parts an
	// unknown key would carry are silently DROPPED (verified: a bogus key is accepted without
	// complaint, so "accepted" does not mean "used").
	promptStyle string
	// modelFieldName is the field carrying the model id inside the model object: v1 `modelID`,
	// v2 `id` (Model.Ref = {id, providerID, variant?}).
	modelFieldName string
	// setModelPath builds the route that sets a session's model, for surfaces whose PROMPT body
	// cannot carry it. Empty on v1, where the model rides each prompt.
	setModelPath func(sessionID string) string
	// setAgentPath builds the route that switches a session's AGENT, or is nil on a surface with no
	// such route (v1).
	setAgentPath func(sessionID string) string
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

// shapeV1 is the surface the adapter was written against, and the DEFAULT.
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
	promptStyle:           "parts",
	modelFieldName:        "modelID",
}

// THERE IS NO "v1+api-prompt" SHAPE, AND ITS ABSENCE IS THE FIX.
//
// It existed for opencode 1.18+, which serves POST /api/session/{id}/prompt alongside its v1 routes, and it sent
// every prompt there so an interjection could be steered into a running turn. Measured on 1.19.5, that route
// ADMITS a message and never runs it:
//
//	POST /api/session/{id}/prompt {"prompt":{"text":"Say pong"}}
//	  -> 200 {"data":{"admittedSeq":2,"id":"msg_…","delivery":"steer"}}     (nothing ever generates)
//	POST /session/{id}/prompt_async {"parts":[…],"model":{…}}
//	  -> 204, and the assistant replies "pong" within ~5 seconds
//
// The same result with resume:true and with a session created via /api/session, so the route is not a start
// path at all — it delivers a steer into a turn that is ALREADY running. With nothing running the message queued
// forever: the client saw 200 + an id, called the send done, and the turn hung until
// `stalled:no_progress (5m0s with no activity)`.
//
// So 1.x uses v1's prompt_async, exactly as before the shape was added, and steering is back to the
// queued-at-the-boundary behaviour the v1 path implements and warns about. A NICETY IS NOT WORTH TURNS THAT
// NEVER START.

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
	codeMode:              true,
	prefix:                "/api",
	healthPath:            "/api/info",
	promptSuffix:          "prompt",
	abortSuffix:           "interrupt",
	compactSuffix:         "compact",
	permissionReplyNested: false,
	permissionReplyField:  "decision",
	responseEnvelope:      "data",
	modelsArgs:            []string{"models"},
	promptStyle:           "text",
	modelFieldName:        "id",
	// v2's prompt body has no model field, so the model is set on the SESSION first
	// (session.switchModel, audit 055: POST /api/session/{sessionID}/model, body {model: Model.Ref}).
	setModelPath: func(id string) string { return "/api/session/" + id + "/model" },
	// session.switchAgent (audit 054: POST /api/session/{sessionID}/agent, body {agent: Agent.ID}).
	setAgentPath: func(id string) string { return "/api/session/" + id + "/agent" },
}

// Path builders. The client calls these and never concatenates a route itself — that is what keeps a
// version difference from leaking into call sites.

func (s apiShape) sessionsPath() string { return s.prefix + "/session" }

func (s apiShape) sessionPath(escapedID string) string { return s.prefix + "/session/" + escapedID }

func (s apiShape) promptPath(sessionID string) string {
	if s.promptPathOverride != nil {
		return s.promptPathOverride(sessionID)
	}
	return s.plainPromptPath(sessionID)
}

// plainPromptPath is the surface's OWN prompt route, ignoring any override — the fallback a 1.x client
// uses when a serve answers the prompt route with its web app instead of admitting a message.
func (s apiShape) plainPromptPath(sessionID string) string {
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

// THE MAJOR IS THE ONLY THING THAT DECIDES.
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
	// EVERY 1.x USES THE V1 PROMPT ROUTE, INCLUDING THE VERSIONS THAT ALSO SERVE /api/session/{id}/prompt.
	//
	// That route exists on 1.18+, and it was chosen here so an interjection could be STEERED into a running
	// turn. Measured on 1.19.5, it cannot do the job it was chosen for — and worse, choosing it stopped turns
	// from starting at all:
	//
	//	POST /api/session/{id}/prompt  {"prompt":{"text":"Say pong"}}              -> 200
	//	  {"data":{"admittedSeq":2,"id":"msg_…","delivery":"steer"}}               <- ADMITTED, never run
	//	  (also tried: resume:true, a session created via /api/session — no generation, 40s+, no reply,
	//	   nothing in the serve's own log)
	//	POST /session/{id}/prompt_async {"parts":[…],"model":{…}}                  -> 204, and the assistant
	//	  replies "pong" within ~5s
	//
	// So this route QUEUES a message and only ever delivers it as a steer into a turn that is already running.
	// With nothing running, the message sat in the queue forever: the client saw 200 + an id and considered the
	// send done, and the turn hung until `stalled:no_progress (5m0s with no activity)`.
	//
	// CORRECT TURNS BEAT A NICETY. Steering on 1.x goes back to the queued-at-the-turn-boundary behaviour the
	// v1 fallback already implements and warns about — which is what actually worked before the shape existed.
	// Restoring steering needs a route that STARTS a turn and steers into it, and 1.x does not have one.
	return shapeV1, true
}

// minorOf parses the minor version of a "MAJOR.MINOR[.PATCH]" string.
func minorOf(v string) (int, bool) {
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, false
	}
	return n, true
}

// detectAPIShape asks the binary which surface it serves.
func detectAPIShape(ctx context.Context, binary string) (shape apiShape, version string, ok bool) {
	if strings.TrimSpace(binary) == "" {
		return shapeV1, "", false
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	out, err := procwin.CommandContext(cctx, binary, "--version").Output()
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
// binary's API surface requires. EXPORTED BECAUSE THE RULE MUST NOT BE RE-DERIVED.
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

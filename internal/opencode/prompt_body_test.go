package opencode

// prompt_body_test.go — THE PROMPT BODY IS NOT THE SAME ON BOTH SURFACES.
//
// The reported failure:
//
//	opencode serve POST /api/session/ses_…/prompt: http 400:
//	{"_tag":"InvalidRequestError","message":"Missing key\n at [\"text\"]","kind":"Payload"}
//
// v1 sends a `parts` array; v2 REQUIRES a top-level `text`. From the OpenAPI spec the v2 audit names
// as its source (packages/protocol/openapi.json, POST /api/session/{id}/prompt):
//
//	{"text": string, "files": [FileAttachment], "agents": […], "skills": […],
//	 "metadata": {}, "delivery": "steer"|"queue", "resume": bool}
//	required: ["text"], additionalProperties: false
//
// There is NO `system`, NO `model` and NO `parts`. Two consequences beyond the 400:
//
//   * `model` cannot ride the prompt, so it is set on the SESSION (session.switchModel). Measured
//     shape: POST /api/session/{id}/model with {"model":{"id":…,"providerID":…}} — note `id`, not
//     v1's `modelID`.
//   * `system` has NO home on any v2 request body (checked across all 118 paths in the spec), so the
//     per-message system prompt is not deliverable on that surface. That is a KNOWN GAP, and the
//     client says so in the log rather than dropping it silently.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// captureServer records each request's method, path and decoded body, answering 200 with a plausible
// body so the client's decode also succeeds.
func captureServer(t *testing.T, seen *[]capturedReq) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&b)
		}
		*seen = append(*seen, capturedReq{method: r.Method, path: r.URL.Path, body: b})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":"ses_x"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

type capturedReq struct {
	method string
	path   string
	body   map[string]any
}

// promptRequest returns the request that carried the MESSAGE.
//
// It matches the prompt ROUTE rather than "the first POST", and that distinction is not pedantry: on
// v2 the model is set with a POST before the prompt, so returning the first POST handed back the
// wrong request and made the ordering assertion below compare a request with itself. A helper that
// answers the wrong question makes the test lie.
func promptRequest(t *testing.T, seen []capturedReq) capturedReq {
	t.Helper()
	for _, r := range seen {
		if r.method == http.MethodPost && strings.Contains(r.path, "/prompt") {
			return r
		}
	}
	t.Fatalf("no POST to a prompt route was made; saw %+v", seen)
	return capturedReq{}
}

// indexOf returns the position of the first request matching path, or -1.
func indexOf(seen []capturedReq, path string) int {
	for i, r := range seen {
		if r.path == path {
			return i
		}
	}
	return -1
}

func TestV1PromptUsesPartsWithSystemAndModel(t *testing.T) {
	var seen []capturedReq
	srv := captureServer(t, &seen)

	c := NewSessionClient(srv.URL, "", "")
	if got := c.APIShapeName(); got != "v1" {
		t.Fatalf("default shape = %q, want v1", got)
	}
	// A model ref in the form the adapter resolves: adapter/provider/model.
	if err := c.SendMessage(context.Background(), "ses_1", "be terse", "opencode/opencode/big-pickle", "hi"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	got := promptRequest(t, seen)
	if got.path != "/session/ses_1/prompt_async" {
		t.Errorf("path = %q, want the v1 route", got.path)
	}
	if _, ok := got.body["parts"]; !ok {
		t.Errorf("the v1 body must carry `parts`, got %v", got.body)
	}
	if got.body["system"] != "be terse" {
		t.Errorf("the v1 body must carry the system prompt, got %v", got.body["system"])
	}
	m, _ := got.body["model"].(map[string]any)
	if m == nil || m["modelID"] == "" {
		t.Errorf("the v1 model object must use `modelID`, got %v", got.body["model"])
	}
}

func TestV2PromptUsesTopLevelText(t *testing.T) {
	// THE REGRESSION PIN: this body is what produced the 400.
	var seen []capturedReq
	srv := captureServer(t, &seen)

	c := NewSessionClient(srv.URL, "", "")
	c.setAPIShape(shapeV2)

	if err := c.SendMessage(context.Background(), "ses_1", "", "", "hi"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	got := promptRequest(t, seen)
	if got.path != "/api/session/ses_1/prompt" {
		t.Errorf("path = %q, want the v2 route", got.path)
	}
	if got.body["text"] != "hi" {
		t.Errorf("the v2 body must carry a top-level `text` — its ABSENCE is the 400 the operator "+
			"saw (Missing key at [\"text\"]); got %v", got.body)
	}
	// `parts` must NOT be sent: the schema is additionalProperties:false, and an unknown key is
	// silently DROPPED by the server rather than rejected, so a body that still carried `parts`
	// would look accepted while carrying nothing.
	if _, ok := got.body["parts"]; ok {
		t.Errorf("the v2 body must NOT carry `parts` — v2 has no such field and would drop it "+
			"silently; got %v", got.body)
	}
	// AND IT MUST ASK TO STEER. Omitting `delivery` leaves the serve to pick, and what it picked was
	// the behaviour the operator reported: the message was not seen until the turn was over. "steer"
	// runs the input at the NEXT STEP BOUNDARY of the running turn; "queue" parks it behind the turn.
	if got.body["delivery"] != "steer" {
		t.Errorf("the v2 prompt body must carry delivery:\"steer\" so a mid-turn message joins the "+
			"running turn at its next step rather than queueing until the turn ends; got %v",
			got.body["delivery"])
	}
}

func TestV2SetsTheModelOnTheSession(t *testing.T) {
	// v2's prompt body cannot carry a model, so the model must be set on the session first — or the
	// turn runs on the serve's default model rather than the one the operator chose.
	var seen []capturedReq
	srv := captureServer(t, &seen)

	c := NewSessionClient(srv.URL, "", "")
	c.setAPIShape(shapeV2)

	if err := c.SendMessage(context.Background(), "ses_1", "", "opencode/opencode/big-pickle", "hi"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	var setModel *capturedReq
	for i := range seen {
		if seen[i].path == "/api/session/ses_1/model" {
			setModel = &seen[i]
		}
	}
	if setModel == nil {
		t.Fatalf("no POST to the switchModel route was made; saw %+v — without it the turn runs on "+
			"the serve's default model, not the one that was chosen", seen)
	}
	// Model.Ref uses `id`, not v1's `modelID`. Getting this wrong is silent: the server would
	// ignore an unknown key and use its default.
	m, _ := setModel.body["model"].(map[string]any)
	if m == nil {
		t.Fatalf("switchModel body must be {model: Model.Ref}, got %v", setModel.body)
	}
	if m["id"] != "big-pickle" {
		t.Errorf("Model.Ref.id = %v, want big-pickle — v2 names this field `id`, not `modelID`", m["id"])
	}
	if m["providerID"] != "opencode" {
		t.Errorf("Model.Ref.providerID = %v, want opencode", m["providerID"])
	}
	if _, ok := m["modelID"]; ok {
		t.Errorf("Model.Ref must NOT use v1's `modelID` key; got %v", m)
	}
	// And the model must be set BEFORE the prompt, so THIS turn uses it. Asserted by index rather
	// than by presence, because both requests exist either way — the order is the whole point.
	iModel := indexOf(seen, "/api/session/ses_1/model")
	iPrompt := indexOf(seen, promptRequest(t, seen).path)
	if iModel < 0 || iPrompt < 0 {
		t.Fatalf("expected both requests; model at %d, prompt at %d", iModel, iPrompt)
	}
	if iModel > iPrompt {
		t.Errorf("the model was set at request %d but the prompt was sent at %d — the turn ran on "+
			"the serve's default model, not the chosen one", iModel, iPrompt)
	}
}

func TestV2FilesUseTheUriShape(t *testing.T) {
	// v1 carried bytes as a data: URL inside a `parts` entry; v2 takes a `uri`, and a data: URL is a
	// uri — so the encoding travels, under a different key and field name.
	var seen []capturedReq
	srv := captureServer(t, &seen)

	c := NewSessionClient(srv.URL, "", "")
	c.setAPIShape(shapeV2)

	err := c.SendMessageWithAttachments(context.Background(), "ses_1", "", "",
		"look", []AttachmentPart{{Name: "n.txt", MimeType: "text/plain", Data: []byte("hi")}})
	if err != nil {
		t.Fatalf("SendMessageWithAttachments: %v", err)
	}

	got := promptRequest(t, seen)
	files, ok := got.body["files"].([]any)
	if !ok || len(files) != 1 {
		t.Fatalf("the v2 body must carry `files`, got %v", got.body)
	}
	f, _ := files[0].(map[string]any)
	if f == nil || f["uri"] == "" {
		t.Errorf("FileAttachment requires `uri`, got %v", files[0])
	}
	if f["name"] != "n.txt" {
		t.Errorf("FileAttachment.name = %v, want n.txt", f["name"])
	}
	if _, ok := f["url"]; ok {
		t.Errorf("v2 uses `uri`, not v1's `url`; got %v", f)
	}
}

// TestV1APIPromptSteersWithNestedBody pins the 1.x steer wire, measured against a real 1.19.4 serve:
// the content nests under `prompt`, `delivery` is its SIBLING, and the model is set on the SESSION
// because this body has no model field (the serve's own reply echoes {"prompt":{"text":…}} alone).
func TestV1APIPromptSteersWithNestedBody(t *testing.T) {
	var seen []capturedReq
	srv := captureServer(t, &seen)

	c := NewSessionClient(srv.URL, "", "")
	c.setAPIShape(shapeV1APIPrompt)

	if err := c.SendMessage(context.Background(), "ses_1", "", "opencode/opencode/big-pickle", "hi"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	got := promptRequest(t, seen)
	if got.path != "/api/session/ses_1/prompt" {
		t.Errorf("prompt path = %q, want the 1.x steer route /api/session/ses_1/prompt", got.path)
	}
	if got.body["delivery"] != "steer" {
		t.Errorf("delivery = %v, want \"steer\" — omitting it leaves the serve to queue the message "+
			"until the turn ends, which is the reported bug", got.body["delivery"])
	}
	p, ok := got.body["prompt"].(map[string]any)
	if !ok {
		t.Fatalf("body has no nested `prompt` object; got %v — 1.x answers "+
			"`Missing key at [\"prompt\"]` for a flat body", got.body)
	}
	if p["text"] != "hi" {
		t.Errorf("prompt.text = %v, want \"hi\"", p["text"])
	}
	// THE MODEL CANNOT RIDE THIS BODY, so it must have been set on the session FIRST — otherwise the
	// turn runs on the serve's default model rather than the one the operator chose.
	modelAt := indexOf(seen, "/api/session/ses_1/model")
	promptAt := -1
	for i, r := range seen {
		if r.path == "/api/session/ses_1/prompt" {
			promptAt = i
		}
	}
	if modelAt == -1 {
		t.Fatalf("the model was never set on the session; saw %+v", seen)
	}
	if modelAt > promptAt {
		t.Errorf("the model was set AFTER the prompt — the turn would have run on the serve's default model")
	}
}

// aWebAppServer answers the /api prompt route the way a 1.x serve answers an UNKNOWN route: 200 and its
// web app. That is the case a status check cannot see, and the reason the fallback decodes the reply.
func aWebAppServer(t *testing.T, seen *[]capturedReq) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&b)
		}
		*seen = append(*seen, capturedReq{method: r.Method, path: r.URL.Path, body: b})
		if r.URL.Path == "/api/session/ses_1/prompt" {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<!doctype html><html><body>the app</body></html>"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":"ses_x"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestV1APIPromptFallsBackToPromptAsyncWhenTheRouteIsAbsent is the safety net that makes the version
// floor non-critical: if a 1.x serve does NOT carry the /api prompt route, its catch-all answers 200
// with HTML. Treating that as success would SILENTLY DROP the operator's message, so the reply is
// decoded and anything that is not a prompt admission falls back to prompt_async.
func TestV1APIPromptFallsBackToPromptAsyncWhenTheRouteIsAbsent(t *testing.T) {
	var seen []capturedReq
	srv := aWebAppServer(t, &seen)

	c := NewSessionClient(srv.URL, "", "")
	c.setAPIShape(shapeV1APIPrompt)

	if err := c.SendMessage(context.Background(), "ses_1", "be terse", "", "hi"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	// The message must have been delivered the v1 way.
	var delivered *capturedReq
	for i, r := range seen {
		if r.path == "/session/ses_1/prompt_async" {
			delivered = &seen[i]
			break
		}
	}
	if delivered == nil {
		t.Fatalf("the prompt never reached prompt_async, so the message was LOST to the catch-all; saw %+v", seen)
	}
	if _, ok := delivered.body["parts"]; !ok {
		t.Errorf("the fallback body must be the v1 shape (`parts`); got %v", delivered.body)
	}

	// AND IT IS REMEMBERED: the next send must not repeat the doomed attempt.
	before := len(seen)
	if err := c.SendMessage(context.Background(), "ses_1", "be terse", "", "again"); err != nil {
		t.Fatalf("second SendMessage: %v", err)
	}
	for _, r := range seen[before:] {
		if r.path == "/api/session/ses_1/prompt" {
			t.Error("the client re-attempted the absent /api route — apiPromptOff should make every later " +
				"send go straight to prompt_async")
		}
	}
}

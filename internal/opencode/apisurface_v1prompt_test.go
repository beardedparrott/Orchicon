package opencode

// apisurface_v1prompt_test.go — A 1.x TURN MUST GO ON THE ROUTE THAT STARTS IT.
//
// REPORTED on a macOS box with opencode 1.19.5: opencode runs hung. Not an error — the drop-down just sat
// there. The plane's log named it:
//
//	ask orchicon turn stalled  reason=stalled:no_progress (5m0s with no activity from model …)
//
// The model call had already been fixed (the set-model route wanted `id`), so the prompt was accepted. It was
// accepted by the WRONG route. Measured against a real 1.19.5 serve:
//
//	POST /api/session/{id}/prompt {"prompt":{"text":"Say pong"}}
//	  -> 200 {"data":{"admittedSeq":2,"id":"msg_…","delivery":"steer"}}   and NOTHING ever generates
//	     (same with resume:true; same on a session created via /api/session; nothing in the serve log)
//	POST /session/{id}/prompt_async {"parts":[…],"model":{…}}
//	  -> 204, and the assistant answers "pong" within ~5 seconds
//
// So that route only delivers a STEER into a turn that is already running. Used as the send path it queues the
// message behind a turn that never starts — and because it answers 200 with an id, the client counted the send
// as done and waited until the stall detector killed the turn.
//
// These tests pin the routing so it cannot come back: 1.x sends to prompt_async, 2.x sends to /api/…/prompt.

import (
	"context"
	"testing"
)

// EVERY 1.x VERSION SENDS ITS PROMPT TO prompt_async, the only route that STARTS a turn on 1.x.
func TestV1PromptsGoToTheRouteThatStartsTheTurn(t *testing.T) {
	for _, version := range []string{"1.17.9", "1.18.34", "1.19.4", "1.19.5"} {
		sh, parsed := apiShapeForVersion(version)
		if !parsed {
			t.Fatalf("version %q did not parse", version)
		}
		if sh.name != "v1" {
			t.Errorf("version %q resolved to shape %q, want v1 — every 1.x must use prompt_async, because "+
				"/api/session/{id}/prompt admits a message and never runs the turn", version, sh.name)
		}
		if got := sh.promptPath("ses_x"); got != "/session/ses_x/prompt_async" {
			t.Errorf("version %q sends its prompt to %q, want /session/ses_x/prompt_async — the only route "+
				"measured to start a turn on 1.x", version, got)
		}
	}
}

// AND THE MODEL RIDES THAT BODY, so no session set-model call is needed on 1.x at all.
func TestV1CarriesTheModelInThePromptBody(t *testing.T) {
	sh, _ := apiShapeForVersion("1.19.5")
	if sh.setModelPath != nil {
		t.Errorf("1.x resolves to a shape that sets the model on the session — prompt_async carries the model "+
			"in its own body, and the session route belongs to the 2.x wire")
	}
	if sh.modelFieldName != "modelID" {
		t.Errorf("1.x prompt body field = %q, want modelID (the prompt_async contract)", sh.modelFieldName)
	}
}

// 2.x IS UNCHANGED: it has no model in the prompt body, so it sets it on the session first.
func TestV2StillSetsTheModelOnTheSession(t *testing.T) {
	sh, _ := apiShapeForVersion("2.0.26")
	if sh.name != "v2" || sh.setModelPath == nil || sh.modelFieldName != "id" {
		t.Fatalf("2.x shape drifted: name=%q setModelPath=%v field=%q", sh.name, sh.setModelPath != nil,
			sh.modelFieldName)
	}
}

// THE WIRE ITSELF, at the client: a 1.x client must never POST a prompt to the /api route.
func TestAV1ClientNeverPostsAPromptToTheAPIroute(t *testing.T) {
	var seen []capturedReq
	srv := captureServer(t, &seen)
	c := NewSessionClient(srv.URL, "", "")
	sh, _ := apiShapeForVersion("1.19.5")
	c.setAPIShape(sh)

	if err := c.SendMessage(context.Background(), "ses_1", "", "opencode/opencode-go/deepseek-v4.1-flash", "hi"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	for _, r := range seen {
		if r.path == "/api/session/ses_1/prompt" {
			t.Fatalf("a 1.x client POSTed its prompt to %q — that route admits and never runs, which is the "+
				"hang this test exists to prevent; saw %+v", r.path, seen)
		}
	}
	got := promptRequest(t, seen)
	if got.path != "/session/ses_1/prompt_async" {
		t.Fatalf("prompt went to %q, want /session/ses_1/prompt_async", got.path)
	}
	if _, ok := got.body["parts"]; !ok {
		t.Errorf("the v1 body must carry `parts`; got %v", got.body)
	}
	if _, ok := got.body["model"]; !ok {
		t.Errorf("the v1 body must carry the model; got %v", got.body)
	}
}

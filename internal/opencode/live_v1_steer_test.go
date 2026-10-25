package opencode

// live_v1_steer_test.go — the 1.x steer wire, proven against a REAL serve rather than a fake.
//
// THE UNIT TESTS ASSERT THE BODY; THIS ASSERTS THE SERVE TAKES IT. The two are different claims: a
// fake server accepts whatever we send, so only a real 1.x serve can show that `delivery:"steer"` and the
// nested `prompt` object are the shapes it actually admits. That is exactly the evidence that was missing
// when the field was silently omitted for months.
//
// OPT-IN, because it needs a 1.x serve to talk to and there is none in CI. It mirrors the package's other
// live tests (ask_mcp_live_test.go): unset env = skip, never a false pass.
//
//	ORCHICON_V1_SERVE_URL=http://127.0.0.1:PORT \
//	ORCHICON_V1_SESSION_ID=ses_… \
//	go test ./internal/opencode/ -run TestLiveV1SteerIsAdmitted -v
//
// The session must already exist on that serve — creating one needs a project directory the serve accepts,
// which is the harness's business rather than this test's.

import (
	"context"
	"os"
	"testing"
)

func TestLiveV1SteerIsAdmitted(t *testing.T) {
	url := os.Getenv("ORCHICON_V1_SERVE_URL")
	sid := os.Getenv("ORCHICON_V1_SESSION_ID")
	if url == "" || sid == "" {
		t.Skip("set ORCHICON_V1_SERVE_URL and ORCHICON_V1_SESSION_ID to run this against a real 1.x serve")
	}
	c := NewSessionClient(url, "", "")
	c.setAPIShape(shapeV1APIPrompt)

	// A plain send: the route must exist and admit it. If the serve answered with its web app instead,
	// the client would fall back to prompt_async — which would ALSO succeed, so this alone cannot tell
	// them apart. The assertion below is what does.
	if err := c.SendMessage(context.Background(), sid, "", "", "live steer probe"); err != nil {
		t.Fatalf("SendMessage against the real 1.x serve: %v", err)
	}
	if c.apiPromptOff.Load() {
		t.Fatalf("the serve answered the /api prompt route with something that is not a prompt admission — "+
			"apiPromptOff was set, so the steer body was NOT accepted by this serve (%s)", url)
	}
	if _, err := c.postNestedPrompt(context.Background(), c.shape.promptPath(sid),
		map[string]any{"prompt": map[string]any{"text": "steer admission probe"}, "delivery": "steer"}); err != nil {
		t.Fatalf("the steer body was rejected outright: %v", err)
	}
}

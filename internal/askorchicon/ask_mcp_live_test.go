package askorchicon

// ask_mcp_live_test.go — the REAL-TURN end-to-end proof for child 6, the one the
// acceptance criteria actually demand ("observed on a real turn, not asserted only
// in a unit test"; "a call to one succeeds"; "verified per mode with a real turn").
//
// WHAT MAKES IT REAL, and why the other file is not enough. ask_mcp_test.go drives
// AskToolDefs/ExecuteAskTool with a FAKE resolver and a FAKE client. This file
// replaces every seam with the production one:
//
//   - a REAL `mcp_servers` row (project-owned AND conversation-owned) written to
//     Postgres, read back by the REAL mcpsettings.Resolver (mcpsettings.NewResolver);
//   - a REAL MCP server process (an in-process streamable-HTTP server built with the
//     same go-sdk the manager speaks to), so discovery and an actual tool call cross
//     a real JSON-RPC transport;
//   - the REAL native Ask turn: `NativeBridge.SendTurnMessage` with the injected
//     askorchicon AskToolProvider, over a scripted provider whose FIRST round asks
//     the model to call `mcp__<server>__<tool>` and whose SECOND round consumes the
//     tool result — so the call is executed by the bridge's own tool loop
//     (chatturn.go executeToolCalls), exactly as a live conversation would.
//
// Gate: ORCHICON_TEST_DSN (a disposable database), the repo-wide convention.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/mcpsettings"
	"github.com/beardedparrott/orchicon/internal/orchicon"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// --- a REAL MCP server, over streamable HTTP (no child process, no network) -----

// liveMCPServerTool is the tool the fixture server advertises. Its name is what the
// platform cannot classify — the whole point of the opaque-MCP rule.
const liveMCPServerTool = "create_issue"

// newLiveMCPFixture starts an in-process streamable-HTTP MCP server and returns its
// URL. The manager connects to it for real (list + call cross the wire).
func newLiveMCPFixture(t *testing.T) string {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "qa-live", Version: "0.0.1"}, nil)
	srv.AddTool(&mcp.Tool{
		Name:        liveMCPServerTool,
		Description: "Open an issue (a third-party action the platform cannot classify).",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"title": map[string]any{"type": "string"}},
			"required":   []string{"title"},
		},
	}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct{ Title string }
		_ = json.Unmarshal(req.Params.Arguments, &args)
		return &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.TextContent{Text: "opened issue: " + args.Title + " (LIVE)"},
		}}, nil
	})
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	t.Cleanup(ts.Close)
	return ts.URL
}

// --- a REAL scope: project-owned + conversation-owned mcp_servers rows ----------

// seedLiveMCPRows writes one project-owned server and one conversation-owned server
// into the given tenant and returns their ids (used as the `mcp__<id>__<tool>`
// server prefix).
func seedLiveMCPRows(t *testing.T, pool *db.Pool, url string) (convID, projectID, convServerID, projServerID string) {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, workItemKindTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: workItemKindTestTenant, Name: "Ask MCP Live",
		Slug: "ask-mcp-live-" + strings.ToLower(db.NewID()), Status: "active",
		Goals: []byte("[]"), ProjectDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	conv, err := db.CreateConversation(ctx, ttx.Tx, db.ConversationRow{
		ID: db.NewID(), TenantID: workItemKindTestTenant, ProjectID: proj.ID,
	})
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	projServerID = "liveproj" + strings.ToLower(db.NewID())[:6]
	convServerID = "liveconv" + strings.ToLower(db.NewID())[:6]
	if _, err := db.UpsertMCPServer(ctx, ttx.Tx, db.MCPServerRow{
		ID: projServerID, TenantID: workItemKindTestTenant, Name: projServerID,
		Transport: "streamable-http", URL: url, Enabled: true, ProjectID: proj.ID,
		Env: map[string]string{}, Headers: map[string]string{}, InstallResult: []byte("{}"),
	}); err != nil {
		t.Fatalf("upsert project-owned server: %v", err)
	}
	if _, err := db.UpsertMCPServer(ctx, ttx.Tx, db.MCPServerRow{
		ID: convServerID, TenantID: workItemKindTestTenant, Name: convServerID,
		Transport: "streamable-http", URL: url, Enabled: true, ConversationID: conv.ID,
		Env: map[string]string{}, Headers: map[string]string{}, InstallResult: []byte("{}"),
	}); err != nil {
		t.Fatalf("upsert conversation-owned server: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return conv.ID, proj.ID, convServerID, projServerID
}

// --- a scripted provider: round 1 calls the MCP tool, round 2 speaks -------------

// liveTurnProvider streams round 1 (a tool call for `tool`) then round 2 (a reply
// that quotes the tool result), and records the tool defs it was OFFERED so the test
// can assert on the offered surface too.
type liveTurnProvider struct {
	tool string
	// seenTools is the def-name set offered on the round-1 request.
	seenTools []string
	round     int
}

func (p *liveTurnProvider) StreamTurn(_ context.Context, req orchicon.TurnRequest) (orchicon.TurnStream, error) {
	p.round++
	switch p.round {
	case 1:
		names := make([]string, 0, len(req.Tools))
		for _, d := range req.Tools {
			names = append(names, d.Name)
		}
		p.seenTools = names
		return newLiveStream([]orchicon.Event{
			orchicon.ToolCall{Index: 0, ToolCallID: "live-call-1", Name: p.tool, ArgsJSON: `{"title":"live-issue"}`},
			orchicon.Finish{StopReason: orchicon.StopToolUse},
		}), nil
	default:
		// The tool result is the last message in the re-sent history; quote it so the
		// assertion can prove the call's REAL output reached the model.
		out := ""
		for _, m := range req.Messages {
			for _, c := range m.Content {
				if c.ToolResult != nil {
					out = c.ToolResult.Content
				}
			}
		}
		return newLiveStream([]orchicon.Event{
			orchicon.TextDelta{Text: "the server said: " + out},
			orchicon.Finish{StopReason: orchicon.StopStop},
		}), nil
	}
}

func (p *liveTurnProvider) ListModels(context.Context) ([]orchicon.ModelInfo, error) { return nil, nil }
func (p *liveTurnProvider) Capabilities() orchicon.Capabilities {
	return orchicon.Capabilities{Streaming: true, Tools: true}
}

type liveStream struct {
	events []orchicon.Event
	i      int
}

func newLiveStream(evts []orchicon.Event) *liveStream { return &liveStream{events: evts} }
func (s *liveStream) Next(context.Context) (orchicon.Event, bool, error) {
	if s.i < len(s.events) {
		e := s.events[s.i]
		s.i++
		return e, true, nil
	}
	return nil, false, nil
}
func (s *liveStream) Close() error { return nil }

// --- the tests -------------------------------------------------------------------

// AC 2 (native leg) + AC 3, ON A REAL TURN: a conversation in a project receives the
// project's server AND its own, both discovered over a real transport, offered in the
// mode that may act, and a call to one is executed by the bridge's tool loop and its
// REAL output returned to the model.
func TestLiveAskTurnReceivesProjectAndConversationMCPInEveryMode(t *testing.T) {
	pool := workItemKindTestPool(t)
	url := newLiveMCPFixture(t)
	convID, projectID, convServerID, projServerID := seedLiveMCPRows(t, pool, url)

	svc := New(pool, slog.Default(), nil, nil, nil)
	svc.SetScopeResolver(mcpsettings.NewResolver(pool))

	// AC 2 "in EVERY mode": the SKILLS half and the scope stamping are mode-independent;
	// for MCP the mode decides OFFER vs REFUSE. Assert the OFFERED surface per mode for
	// an Iteration turn here (act), and prove the refusal path per mode in the sibling
	// test below.
	prov := &liveTurnProvider{tool: mcpclient.ToolName(convServerID, liveMCPServerTool)}
	b := orchicon.NewBridge(orchicon.ProviderResolverFunc(func(context.Context, string, string) (orchicon.Provider, error) {
		return prov, nil
	}), "", slog.New(slog.NewTextHandler(&strings.Builder{}, nil)))
	b.SetAskTools(svc.NativeAskTools())

	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	ctx = withAskMode(ctx, modeIteration)
	ctx = withAskConversation(ctx, convID)
	ctx = withAskConversationProject(ctx, projectID)

	sid, err := b.CreateConversationSession(ctx, convID, "live")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	bus, err := b.Subscribe(ctx, convID)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := b.SendTurnMessage(ctx, convID, sid, "system", "orchicon/test", "open an issue"); err != nil {
		t.Fatalf("SendTurnMessage: %v", err)
	}
	evts := drainLive(t, b, bus, "once")

	// AC 2: BOTH the project's and the conversation's servers appear in the offered set.
	wantConv := mcpclient.ToolName(convServerID, liveMCPServerTool)
	wantProj := mcpclient.ToolName(projServerID, liveMCPServerTool)
	if !contains(prov.seenTools, wantConv) || !contains(prov.seenTools, wantProj) {
		t.Fatalf("the real turn was not offered BOTH servers' tools: got %v, want %q and %q", prov.seenTools, wantConv, wantProj)
	}

	// AC 3: the call ran and its REAL result came back to the model.
	var toolResult string
	for _, e := range evts {
		if e.Kind == "tool_result" && e.ToolName == wantConv {
			toolResult = e.Output
		}
	}
	if !strings.Contains(toolResult, "opened issue: live-issue (LIVE)") {
		t.Fatalf("the MCP call did not produce its real output: %q (events: %s)", toolResult, liveEventKinds(evts))
	}
	// ...and the model's second round saw it.
	var reply string
	for _, e := range evts {
		if e.Kind == "part" && e.Type == "text" {
			reply += e.Text
		}
	}
	if !strings.Contains(reply, "opened issue: live-issue (LIVE)") {
		t.Fatalf("the tool result did not reach the model's next round: %q", reply)
	}
}

// AC 5, ON A REAL TURN: in a mode that may not act, the opaque MCP tool is NEITHER
// offered NOR executed, and the refusal names the mode and says the assistant cannot
// switch itself. Verified PER MODE (brainstorm + quick_work).
func TestLiveAskTurnRefusesOpaqueMCPPerMode(t *testing.T) {
	pool := workItemKindTestPool(t)
	url := newLiveMCPFixture(t)
	convID, projectID, convServerID, _ := seedLiveMCPRows(t, pool, url)

	svc := New(pool, slog.New(slog.NewTextHandler(&strings.Builder{}, nil)), nil, nil, nil)
	svc.SetScopeResolver(mcpsettings.NewResolver(pool))

	for _, mode := range []string{modeBrainstorm, modeQuickWork} {
		t.Run(mode, func(t *testing.T) {
			prov := &liveTurnProvider{tool: mcpclient.ToolName(convServerID, liveMCPServerTool)}
			b := orchicon.NewBridge(orchicon.ProviderResolverFunc(func(context.Context, string, string) (orchicon.Provider, error) {
				return prov, nil
			}), "", slog.New(slog.NewTextHandler(&strings.Builder{}, nil)))
			b.SetAskTools(svc.NativeAskTools())

			ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
			ctx = withAskMode(ctx, mode)
			ctx = withAskConversation(ctx, convID)
			ctx = withAskConversationProject(ctx, projectID)

			sid, _ := b.CreateConversationSession(ctx, convID+"-"+mode, "live")
			bus, _ := b.Subscribe(ctx, convID+"-"+mode)
			// The call is asked for, but the mode must refuse it. Consent is answered
			// "once" below so the FIRST gate (consent) is not what stops it — the MODE
			// guard is, which is exactly the boundary under test.
			if err := b.SendTurnMessage(ctx, convID+"-"+mode, sid, "system", "orchicon/test", "open an issue"); err != nil {
				t.Fatalf("SendTurnMessage: %v", err)
			}
			evts := drainLive(t, b, bus, "once")

			// NOT OFFERED.
			for _, n := range prov.seenTools {
				if strings.HasPrefix(n, "mcp__"+convServerID+"__") {
					t.Fatalf("%s was OFFERED %q — the offered surface must not advertise an action the mode refuses", mode, n)
				}
			}
			// NOT EXECUTED: the tool result is an error naming the mode.
			var refusal string
			for _, e := range evts {
				if e.Kind == "tool_result" && strings.HasPrefix(e.ToolName, "mcp__") {
					if !e.IsError {
						t.Fatalf("%s ran an opaque MCP tool (IsError=false): %+v", mode, e)
					}
					refusal = e.Output
				}
			}
			if refusal == "" {
				t.Fatalf("%s: no tool_result for the MCP call at all: %s", mode, liveEventKinds(evts))
			}
			for _, want := range []string{"REFUSED BY THE PLATFORM", modeLabel(mode), "switch your own mode", "ASK THE USER"} {
				if !strings.Contains(refusal, want) {
					t.Fatalf("%s refusal is missing %q:\n%s", mode, want, refusal)
				}
			}
		})
	}
}

// drainLive collects a turn's events until the bus closes (bounded).
//
// ONE READER, deliberately. It also answers a consent card inline: an opaque MCP
// tool is consent-gated in EVERY mode (consentGatedTool), so a test that never
// answers would stall on consent's fail-closed window instead of reaching the MODE
// guard under test. A second goroutine reading bus.Events() would SPLIT the event
// stream with this one, so the answering happens HERE rather than beside it.
//
// `consent` selects the answer: "once" approves (so the mode guard is the thing
// that decides), "" leaves every ask unanswered (the fail-closed path).
func drainLive(t *testing.T, b *orchicon.NativeBridge, bus scheduler.SessionBus, consent string) []scheduler.SessionEvent {
	t.Helper()
	var out []scheduler.SessionEvent
	timeout := time.After(20 * time.Second)
	for {
		select {
		case evt, ok := <-bus.Events():
			if !ok {
				return out
			}
			out = append(out, evt)
			if consent != "" && evt.Kind == "permission" && evt.PermissionID != "" {
				// The reply is delivered out of band through the bridge's own registry,
				// so this is not a re-entrant read of the bus.
				_ = b.ReplyPermissionDecision(context.Background(), "live", evt.PermissionID, consent)
			}
		case <-bus.Done():
			for {
				select {
				case evt, ok := <-bus.Events():
					if !ok {
						return out
					}
					out = append(out, evt)
				default:
					return out
				}
			}
		case <-timeout:
			t.Fatalf("drainLive timed out; events so far: %s", liveEventKinds(out))
		}
	}
}

func liveEventKinds(evts []scheduler.SessionEvent) string {
	parts := make([]string, 0, len(evts))
	for _, e := range evts {
		s := e.Kind
		if e.ToolName != "" {
			s += "(" + e.ToolName + ")"
		}
		parts = append(parts, s)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

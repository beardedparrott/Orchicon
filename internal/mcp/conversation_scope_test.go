package mcp

// conversation_scope_test.go — the END-TO-END proof of the conversation-scope fix:
// the REAL `get_current_conversation` tool, served by the REAL `orchicon mcp` sidecar,
// over the REAL stdio path.
//
// The unit tests either side of this pin the links (the claude transport puts the
// conversation id in the child's environment; the sidecar restores it onto the tool's
// context). This file pins the SYMPTOM the operator reported, which is what those links
// exist for: an Orchicon tool in a claude Ask session answering
// "no conversation is stamped on this turn".
//
// The positive case is DB-backed and skips without ORCHICON_TEST_DSN (the repo's
// pattern). The negative case needs no DB — the tool refuses an unidentified session
// BEFORE it opens a transaction — so it always runs, which is the half worth having in
// every suite: it is what proves the fix did not simply make the tool answer anyway.
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5433/orchicon?sslmode=disable'
//	go test ./internal/mcp/ -run TestGetCurrentConversation -v

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/askorchicon"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
)

// The tool resolves the conversation it is serving, carried across the stdio boundary on
// the sidecar's environment.
func TestGetCurrentConversationResolvesThroughTheSidecarWithTheEnvScope(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping the DB-backed sidecar end-to-end test")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := migrate.Run(ctx, pool, assets.MigrationsFS, assets.MigrationsDir); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	// A conversation carrying a model_ref, so the tool has something real to resolve.
	const wantRef = "orchicon/anthropic/claude-sonnet-4"
	convID := seedConversation(t, pool, wantRef)

	t.Setenv("ORCHICON_MCP_CONVERSATION_ID", convID)
	t.Setenv("ORCHICON_MCP_TENANT_ID", "tnt_dev")

	reg := NewAskOrchiconRegistry(askorchicon.NewToolRegistry(pool, quietTestLogger(), nil))
	out := driveToolCall(t, reg, pool, "get_current_conversation")

	if strings.Contains(out, "no conversation is stamped") {
		t.Fatalf("the tool still cannot identify its session — the scope did not survive the stdio boundary: %s", out)
	}
	if strings.Contains(out, `"isError":true`) {
		t.Fatalf("the tool call failed: %s", out)
	}
	if !strings.Contains(out, wantRef) {
		t.Errorf("the tool did not resolve the conversation's model_ref %q: %s", wantRef, out)
	}
	if !strings.Contains(out, convID) {
		t.Errorf("the tool did not report conversation id %q: %s", convID, out)
	}
	if !strings.Contains(out, "conversation") {
		t.Errorf("the tool did not report the ref's source as the conversation: %s", out)
	}
}

// THE NEGATIVE, at the same level and with no DB: a sidecar that serves NO conversation
// must still refuse. This is the honest-refusal half of the contract — a tool that reports
// facts about "this session" must never invent one, and the fix must not have turned the
// loud error into a confident guess.
func TestGetCurrentConversationStillRefusesWithoutTheEnvScope(t *testing.T) {
	t.Setenv("ORCHICON_MCP_CONVERSATION_ID", "")
	t.Setenv("ORCHICON_MCP_TENANT_ID", "tnt_dev")

	// No pool is needed: the tool refuses before it opens a transaction, which is
	// exactly why this case can run everywhere.
	reg := NewAskOrchiconRegistry(askorchicon.NewToolRegistry(nil, quietTestLogger(), nil))
	out := driveToolCall(t, reg, nil, "get_current_conversation")

	if !strings.Contains(out, "no conversation is stamped") {
		t.Fatalf("the tool did not refuse an unidentified session: %s", out)
	}
	if !strings.Contains(out, `"isError":true`) {
		t.Errorf("the refusal was not reported as a tool error: %s", out)
	}
}

// seedConversation inserts a conversation in the dev tenant and returns its id.
func seedConversation(t *testing.T, pool *db.Pool, modelRef string) string {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatalf("begin tenant tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	row, err := db.CreateConversation(ctx, ttx.Tx, db.ConversationRow{
		ID:       db.NewID(),
		TenantID: "tnt_dev",
		ModelRef: modelRef,
	})
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return row.ID
}

func quietTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

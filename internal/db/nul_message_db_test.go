package db_test

// nul_message_db_test.go — the NUL fix against a REAL table.
//
// The pure tests in nul_sanitize_test.go pin what sanitizeMessage does to a row.
// This file pins the thing that actually broke: what PostgreSQL does with a NUL
// on the way IN, for every column the running turn writes through. It is also the
// only place the counterfactual is reproducible — the raw INSERT below is the
// production failure, byte for byte.
//
// Guarded by ORCHICON_TEST_DSN like the other DB-backed tests in this package.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
)

// TestMessageWriteSurvivesANulByte proves, against a real server, that
//
//	(a) a NUL in a jsonb argument is rejected with SQLSTATE 22P05 — the exact
//	    failure that silently killed 1554 live mirrors and 3 terminal replies on
//	    the prod plane, and
//	(b) both message write paths (the 250ms partial upsert and the terminal
//	    create) now store the same data instead of losing the turn.
func TestMessageWriteSurvivesANulByte(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed NUL message test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := migrate.Run(ctx, pool, assets.MigrationsFS, assets.MigrationsDir); err != nil {
		t.Fatal(err)
	}
	const tenant = "tnt_nulmsg"

	// (a) THE COUNTERFACTUAL, in its OWN transaction: the raw write, exactly as the mirror used to make it. A
	// jsonb argument carrying the escaped NUL is refused outright by PostgreSQL — and a refused statement
	// poisons its transaction, so it must not share one with the assertions below.
	{
		rawTx, err := pool.BeginTenantTx(ctx, tenant)
		if err != nil {
			t.Fatal(err)
		}
		conv, err := db.CreateConversation(ctx, rawTx.Tx, db.ConversationRow{
			ID: "conv_nul_raw", TenantID: tenant, Title: "nul", ModelRef: "orchicon/deepseek/deepseek-flash",
		})
		if err != nil {
			t.Fatal(err)
		}
		raw := `INSERT INTO ask_orchicon_messages
			(id, tenant_id, conversation_id, role, content, tool_calls, tool_results, attachments, metadata, reasoning)
			VALUES ($1, $2, $3, 'assistant', $4, $5, '[]', '[]', '{}', '[]')`
		_, rawErr := rawTx.Tx.Exec(ctx, raw, "m_raw", tenant, conv.ID, "text", []byte(`[{"output":"\u0000"}]`))
		if rawErr == nil {
			t.Fatal("the raw write was ACCEPTED — this test no longer reproduces the production failure, so it cannot prove the fix")
		}
		if !strings.Contains(rawErr.Error(), "22P05") {
			t.Fatalf("raw write failed with %v, want SQLSTATE 22P05 (the production error)", rawErr)
		}
		rawTx.Rollback(ctx)
	}

	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	conv, err := db.CreateConversation(ctx, ttx.Tx, db.ConversationRow{
		ID: "conv_nul", TenantID: tenant, Title: "nul", ModelRef: "orchicon/deepseek/deepseek-flash",
	})
	if err != nil {
		t.Fatal(err)
	}

	// (b) The live mirror's path: the growing partial reply, whose tool ledger carries the offending output.
	mirror := db.MessageRow{
		ID:             "m_partial",
		TenantID:       tenant,
		ConversationID: conv.ID,
		Role:           "assistant",
		Content:        "working\x00on it",
		ToolCalls:      []byte(`[{"name":"bash","output":"binary\u0000payload"}]`),
		ToolResults:    []byte(`[{"output":"\u0000"}]`),
		Attachments:    []byte(`[]`),
		Metadata:       []byte(`{"model_ref":"m","session_id":"s\u0000x"}`),
		Reasoning:      []string{"reasoned\x00here"},
	}
	got, err := db.UpsertMessage(ctx, ttx.Tx, mirror)
	if err != nil {
		t.Fatalf("the live mirror still fails on a NUL: %v", err)
	}
	if strings.ContainsRune(got.Content, 0x00) {
		t.Fatalf("stored content still carries a raw NUL: %q", got.Content)
	}
	if !strings.Contains(got.Content, "\uFFFD") {
		t.Fatalf("stored content must show the replacement: %q", got.Content)
	}
	// The rows survive a read back, which is what the client's poll then renders.
	rows, err := db.ListMessages(ctx, ttx.Tx, tenant, conv.ID, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("read back %d messages, want 1", len(rows))
	}
	if !strings.Contains(rows[0].Content, "working") {
		t.Fatalf("the reply did not survive the round trip: %q", rows[0].Content)
	}

	// The terminal write (CreateMessage) takes the same data and must also land.
	if _, err := db.CreateMessage(ctx, ttx.Tx, db.MessageRow{
		ID:             "m_final",
		TenantID:       tenant,
		ConversationID: conv.ID,
		Role:           "assistant",
		Content:        "done\x00",
		ToolCalls:      []byte(`[{"name":"bash","output":"x\u0000y"}]`),
		ToolResults:    []byte(`[]`),
		Attachments:    []byte(`[]`),
		Metadata:       []byte(`{}`),
		Reasoning:      []string{"final\x00"},
	}); err != nil {
		t.Fatalf("the terminal reply still fails on a NUL: %v", err)
	}
}

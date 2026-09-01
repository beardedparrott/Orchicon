package main

import (
	"context"
	"fmt"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/orchicon"
)

func main() {
	ctx := context.Background()
	pool, err := db.Open(ctx, "postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable")
	if err != nil { panic(err) }
	defer pool.Close()
	r := orchicon.NewCredentialResolver(pool, []byte("0123456789abcdef0123456789abcdef"))
	p := orchicon.Profile{ID: "commandcode", AuthSecretRef: "COMMANDCODE_API_KEY", AuthEnv: "COMMANDCODE_API_KEY"}
	tok, err := r.Resolve(ctx, "tnt_providers_test", p)
	fmt.Printf("resolve: tok=%q err=%v\n", tok, err)
	// also direct GetSecretByName probe
	tx, _ := pool.BeginTenantTx(ctx, "tnt_providers_test")
	row, err := db.GetSecretByName(ctx, tx.Tx, "tnt_providers_test", "COMMANDCODE_API_KEY")
	fmt.Printf("getsecret: id=%q err=%v\n", row.ID, err)
	_ = tx.Rollback(ctx)
}

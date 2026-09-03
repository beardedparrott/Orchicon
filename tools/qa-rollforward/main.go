package main

// qa-roll extracts the exact prompt content the canned-worker seeder would
// persist for a given canned worker ID, so out-of-band roll-forwards against
// the live tenant are byte-identical to the seed. Usage:
//
//	qa-roll <canned-worker-id>   (e.g. qa-roll w_se_qa_engineer)
//
// Output format: ===FIELD_*=== sections consumed by the SQL roll-forward
// script. One-off tooling for the QA surface-impact rollout (UPDATES #260);
// safe to delete after the content lands in a release.

import (
	"fmt"
	"os"

	"github.com/beardedparrott/orchicon/internal/db"
)

func main() {
	id := os.Args[1]
	var w *db.CannedWorker
	for i := range db.CannedWorkers() {
		if db.CannedWorkers()[i].ID == id {
			w = &db.CannedWorkers()[i]
		}
	}
	if w == nil {
		fmt.Fprintln(os.Stderr, "canned worker not found:", id)
		os.Exit(1)
	}
	// Exactly what the seeder persists: full AgentsMD for the roll-forward
	// INSERT (the safety rules ship via the stable prompt prefix, not the
	// stored agents_md), and the seed-stripped agents_md.
	fmt.Printf("===FIELD_ROLE===\n%s\n===FIELD_SKILLS===\n%s\n===FIELD_BEHAVIOR===\n%s\n===FIELD_AGENTSMD===\n%s\n===FIELD_SEEDAGENTS===\n%s\n",
		w.Role, w.Skills, w.Behavior, w.AgentsMD, db.SeedAgentsMD(w))
}

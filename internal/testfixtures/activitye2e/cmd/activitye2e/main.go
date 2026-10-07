// Command activitye2e serves the shared disposable plane for the activity-line end-to-end
// capstone: the fixture Ask/Project/Auth Connect services plus the SPA's HTTP auth routes and a
// small harness control surface.
//
//	go run ./internal/testfixtures/activitye2e/cmd/activitye2e -addr 127.0.0.1:18080
//
// The control surface is what lets the BROWSER half drive the phases without reaching into the Go
// process — the two runtimes stay decoupled:
//
//	POST /__e2e/phase?p=flight|started|no-tools|stalled|down|rpc-down
//	POST /__e2e/stamp?v=<unix-ms>
//	POST /__e2e/end
//	POST /__e2e/reset?p=<phase>
//	GET  /__e2e/state   -> {phase, stamp, calls, counter, streamed}
//
// It is a TEST-ONLY artifact by location (internal/testfixtures/, not cmd/), so it is never a
// shipped binary — `make cross-compile` builds only ./cmd/orchicon and ./cmd/orch.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/beardedparrott/orchicon/internal/testfixtures/activitye2e"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18080", "address to bind")
	flag.Parse()

	cwd, err := os.Getwd()
	if err != nil {
		log.Fatalf("activitye2e: getwd: %v", err)
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           activitye2e.Mux(activitye2e.New(), &activitye2e.Sessions{}, cwd),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("activitye2e: serving the fixture plane on http://%s (conv %s)", *addr, activitye2e.ConvID)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("activitye2e: %v", err)
	}
}

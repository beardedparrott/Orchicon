// Command agy-parity-plane serves the antigravity parity fixture plane (child 8, AC3 captures).
//
// USAGE
//
//	agy-parity-plane -addr 127.0.0.1:9099 -state live
//
// It exists so the GUI (Playwright) and the TUI (a real pty) capture the SAME five states from the SAME
// answers, instead of each harness inventing its own. The states are the AC3 list:
// live, not-installed, auth-error, stale, mode-refused.
//
// THIS IS A TEST FIXTURE, NOT A PLANE. It binds a loopback address only and serves no data of its own.
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/beardedparrott/orchicon/internal/testfixtures/antigravityparity"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9099", "loopback address to serve")
	state := flag.String("state", string(antigravityparity.StateLive),
		"AC3 state: live | not-installed | auth-error | stale | mode-refused")
	flag.Parse()

	p := antigravityparity.New(antigravityparity.State(*state))
	srv := &http.Server{
		Addr:              *addr,
		Handler:           antigravityparity.Mux(p),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("agy parity fixture plane: state=%s addr=%s", p.State(), *addr)
	log.Fatal(srv.ListenAndServe())
}

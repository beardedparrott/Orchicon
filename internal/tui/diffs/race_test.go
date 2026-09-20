package diffs

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/subs"
)

// TestSetOwnerFetchClosureConcurrentReads guards against the data race the
// fetch closure used to introduce: bubbletea runs each Cmd in a background
// goroutine (`go func(){ msg := cmd() }()`), so SetOwner's returned fetch
// command must NOT mutate the model. It must only fetch and hand the data
// back in FetchDoneMsg; Update applies it on the tea loop.
//
// The previous implementation mutated m.groups/m.rows/m.Status/m.Err/m.maxSeq
// inside the closure, racing with View()/Update() reads on the tea loop.
// This test drives SetOwner off the calling goroutine (as bubbletea does)
// while a reader goroutine concurrently reads the pane — -race must be clean.
func TestSetOwnerFetchClosureConcurrentReads(t *testing.T) {
	ff := &fakeFileEditService{}
	fp, fph := apiv1connect.NewFileEditServiceHandler(ff)
	fa := &fakeAuthService{}
	ap, aph := apiv1connect.NewAuthServiceHandler(fa)
	mux := http.NewServeMux()
	mux.Handle(fp, fph)
	mux.Handle(ap, aph)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cl := client.NewWithHTTPClient(client.Options{BaseURL: srv.URL, Token: "oc_test"}, srv.Client())
	m := NewModel(cl, subs.NewRegistry())
	m.SetSize(48, 24)
	m.Status = "idle"

	cmd := m.SetOwner("execution", "exec-1", false)
	if cmd == nil {
		t.Skip("no fetch cmd (nil client)")
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 3000; i++ {
			// Mirror the reads View()/Update() perform on the tea loop.
			_ = m.groups
			_ = m.rows
			_ = m.Status
			_ = m.Err
			_ = m.maxSeq
			_ = m.SelectedPath
			_ = m.Width
			_ = m.Height
		}
	}()
	// bubbletea runs this via `go func(){ msg := cmd() }()`; here we execute
	// it on the calling goroutine and consume the msg the way the program's
	// event loop would (handing it to Update on the tea loop).
	_ = cmd()
	wg.Wait()
}

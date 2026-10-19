package desktop

// racecheck_test.go — THE PROXY TARGET IS READ WHILE IT CAN BE SWAPPED.
//
// UsePlane changes which plane the window talks to at runtime. The proxy resolves
// its target PER REQUEST (that is what makes the switch take effect on the next
// API call rather than the next launch), so the field it reads is written
// concurrently with requests in flight. Reading it unguarded is a data race on the
// one field EVERY request touches — `go test -race` reports it, and the fix is a
// lock around the read, not a cached copy of the URL.
//
// This test exists because the race is invisible in ordinary use and invisible to
// the plain test run: it only appears when the operator switches instance while
// the page is polling, which is exactly what Manage Server does.
//
// Run with: go test -race ./internal/desktop/ -run TestProxyFollowsUsePlaneUnderRace

import (
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestProxyFollowsUsePlaneUnderRace drives the exact concurrency the app has:
// requests in flight while the operator switches instance. Run under -race.
func TestProxyFollowsUsePlaneUnderRace(t *testing.T) {
	shell, err := New(Config{
		PlaneURL:   "http://127.0.0.1:1",
		FrontendFS: testSPA(),
		PrefsPath:  filepath.Join(t.TempDir(), "prefs.json"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := shell.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = shell.Close(t.Context()) })

	stop := make(chan struct{})
	var wg sync.WaitGroup
	// Readers: hammer a plane path (it will 502 — we only care about the race).
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				res, err := http.Get(shell.OriginURL() + "/versionz")
				if err == nil {
					_ = res.Body.Close()
				}
			}
		}()
	}
	// Writer: switch instance repeatedly.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			_, _ = shell.UsePlane("prod")
			_, _ = shell.UsePlane("dev")
			time.Sleep(time.Millisecond)
		}
	}()
	time.Sleep(120 * time.Millisecond)
	close(stop)
	wg.Wait()
}

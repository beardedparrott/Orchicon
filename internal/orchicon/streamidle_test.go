package orchicon

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// closableBody emits one byte per Read for the first `chunks` reads, then
// blocks until Close — mirroring a real http response body, whose Close
// unblocks an in-flight Read. That is exactly the mechanism the watchdog's
// fire path relies on; test helpers MUST honor it or the test deadlocks in
// the deferred cleanup (Close waits for the pump, the pump waits in Read).
type closableBody struct {
	chunks int
	pause  time.Duration
	once   sync.Once
	closed chan struct{}
}

func newClosableBody(chunks int, pause time.Duration) *closableBody {
	return &closableBody{chunks: chunks, pause: pause, closed: make(chan struct{})}
}

func (b *closableBody) Read(p []byte) (int, error) {
	if b.chunks > 0 {
		b.chunks--
		if b.pause > 0 {
			time.Sleep(b.pause)
		}
		if len(p) > 0 {
			p[0] = 'x'
		}
		return 1, nil
	}
	<-b.closed
	return 0, errors.New("body closed mid-read")
}

func (b *closableBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func TestIdleWatchBodyIdleAbortsAfterFirstByte(t *testing.T) {
	w := newIdleWatchBody(newClosableBody(1, 0), 80*time.Millisecond)
	defer w.Close()

	buf := make([]byte, 64)
	n, err := w.Read(buf) // first chunk delivers AND arms the watchdog
	if err != nil || n != 1 {
		t.Fatalf("first read = (%d, %v)", n, err)
	}
	start := time.Now()
	_, err = w.Read(buf) // silence → idle abort, NOT a hang
	if !errors.Is(err, ErrStreamIdle) {
		t.Fatalf("second read err = %v, want ErrStreamIdle", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("idle abort took %v — watchdog failed", d)
	}
}

func TestIdleWatchBodyDisabledWhenZero(t *testing.T) {
	rc := io.NopCloser(strings.NewReader("hello"))
	if w := newIdleWatchBody(rc, 0); w != io.ReadCloser(rc) {
		t.Fatal("idle=0 must return the body unchanged (watchdog disabled)")
	}
}

func TestIdleWatchBodyEOFPassesThrough(t *testing.T) {
	w := newIdleWatchBody(io.NopCloser(strings.NewReader("ab")), 5*time.Second)
	defer w.Close()
	var got strings.Builder
	buf := make([]byte, 8)
	for {
		n, err := w.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			if err != io.EOF {
				t.Fatalf("read err = %v", err)
			}
			break
		}
	}
	if got.String() != "ab" {
		t.Fatalf("body = %q", got.String())
	}
}

func TestIdleWatchBodyTimerResetsPerChunk(t *testing.T) {
	// Chunks 150ms apart with a 200ms idle budget: every inter-chunk gap is
	// just UNDER the budget, so if the timer did not re-arm per chunk
	// delivery, it would fire 200ms after the FIRST chunk — mid-gap before
	// chunk 2 arrives — and the second read would fail. Delivering both
	// chunks, then tripping on genuine silence, proves the reset happens.
	// (Gaps larger than the budget are a tripping condition by contract —
	// "no bytes within the budget" — not a reset regression.)
	rc := newClosableBody(2, 150*time.Millisecond)
	w := newIdleWatchBody(rc, 200*time.Millisecond)
	defer w.Close()
	buf := make([]byte, 8)
	for i := 0; i < 2; i++ {
		if _, err := w.Read(buf); err != nil {
			t.Fatalf("chunk %d read err = %v", i, err)
		}
	}
	if _, err := w.Read(buf); !errors.Is(err, ErrStreamIdle) {
		t.Fatalf("silence after chunks err = %v, want ErrStreamIdle", err)
	}
}

func TestIdleWatchBodyShortReadsKeepTail(t *testing.T) {
	// Consumer buffer smaller than the pumped chunk: the tail must be
	// buffered, not dropped (short reads are legal io.Reader semantics —
	// bufio.Scanner starts small and grows).
	w := newIdleWatchBody(io.NopCloser(strings.NewReader("0123456789")), 5*time.Second)
	defer w.Close()
	small := make([]byte, 3)
	var got strings.Builder
	for {
		n, err := w.Read(small)
		got.Write(small[:n])
		if err != nil {
			if err != io.EOF {
				t.Fatalf("read err = %v", err)
			}
			break
		}
	}
	if got.String() != "0123456789" {
		t.Fatalf("short-read body = %q", got.String())
	}
}

func TestPostJSONWrapsIdleWatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	t.Setenv("ORCHICON_STREAM_IDLE_TIMEOUT", "0") // disabled — wrap is a no-op
	httpc := &http.Client{}
	resp, err := postJSON(context.Background(), httpc, srv.URL, nil, []byte(`{}`))
	if err != nil {
		t.Fatalf("postJSON: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(b), `"ok"`) {
		t.Fatalf("body = %s", b)
	}
}

func TestStreamIdleTimeoutEnv(t *testing.T) {
	t.Setenv("ORCHICON_STREAM_IDLE_TIMEOUT", "90s")
	if got := streamIdleTimeout(); got != 90*time.Second {
		t.Fatalf("90s → %v", got)
	}
	t.Setenv("ORCHICON_STREAM_IDLE_TIMEOUT", "0")
	if got := streamIdleTimeout(); got != 0 {
		t.Fatalf("0 → %v", got)
	}
	t.Setenv("ORCHICON_STREAM_IDLE_TIMEOUT", "garbage")
	if got := streamIdleTimeout(); got != defaultStreamIdleTimeout {
		t.Fatalf("garbage → %v, want default", got)
	}
}

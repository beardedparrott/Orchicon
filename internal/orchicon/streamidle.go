package orchicon

import (
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// streamidle.go — the transport-side idle-read watchdog for provider
// streams. History: defaultHTTPClient set http.Client.Timeout = 10m, a
// TOTAL deadline covering the entire request INCLUDING the streamed body
// — and a long prefill (observed: 48k prompt tokens on a local
// llama-server) produces zero body bytes for longer than that, killing a
// healthy generation mid-stream with "stream read: context deadline
// exceeded". The fix splits time-bounding into the phases the transport
// can express honestly:
//
//   - connect/headers: Transport-level timeouts (dial, TLS, response
//     header) — a dead server is detected in seconds-to-minutes;
//   - stream body: NO total cap (a 200k-token generation may legitimately
//     run for hours), but an IDLE watchdog — once the first body byte has
//     arrived, a silent gap longer than the idle budget aborts the read
//     with a typed error. Pre-first-byte silence (the prefill, where the
//     server computes before streaming anything) is EXEMPT by design: it
//     is bounded by the per-execution wall-clock budget instead.
//
// The idle budget is env-tunable: ORCHICON_STREAM_IDLE_TIMEOUT accepts a
// Go duration ("5m", "90s"); "0" disables the watchdog entirely. Default
// 5m — generous against slow chunky local servers, tight enough that a
// wedged connection fails a turn instead of hanging the session until
// the wall-clock reaper.

const defaultStreamIdleTimeout = 5 * time.Minute

// ErrStreamIdle is the typed error surfaced when the stream body stays
// silent past the idle budget (post-first-byte).
var ErrStreamIdle = errors.New("orchicon: stream idle timeout (no bytes from the provider within the idle budget)")

// streamIdleTimeout reads ORCHICON_STREAM_IDLE_TIMEOUT (Go duration).
// Invalid values fall back to the default; "0" disables.
func streamIdleTimeout() time.Duration {
	if v := os.Getenv("ORCHICON_STREAM_IDLE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return defaultStreamIdleTimeout
}

// idleWatchBody wraps a stream body: every delivered chunk re-arms an idle
// timer that aborts the connection on silence. A pump goroutine performs
// the actual reads (a body Read cannot safely be raced against a timer
// from the reader goroutine); the wrapper's Read selects over the pump
// channel and the fired signal. Single-reader contract: the SSE reader is
// the only consumer, so a 1-deep chunk handoff suffices.
//
// Timer discipline: one NEW AfterFunc per chunk (never Timer.Reset — its
// stale-fire semantics would need a generation check anyway). Each timer
// closure captures the generation it armed at; a fire only acts when its
// generation is still current, so a chunk arriving exactly as the idle
// budget expires can never spuriously kill a live stream.
type idleWatchBody struct {
	rc   io.ReadCloser
	idle time.Duration
	ch   chan idleRead
	errc chan error

	// pend holds the unconsumed remainder of a pumped chunk when the
	// consumer's buffer is smaller than the chunk (short reads are legal
	// io.Reader semantics — bufio.Scanner starts with a small buffer and
	// grows; dropping the tail would corrupt the stream).
	pend []byte

	fireOnce sync.Once
	fired    chan struct{} // closed exactly once when the watchdog trips
	gen      atomic.Uint64 // bumped per chunk delivery; timer owners check it

	pumpDone chan struct{} // closed when the pump goroutine exits
	stopOnce sync.Once
	stopped  chan struct{} // closed by Close/teardown
}

type idleRead struct {
	buf []byte
}

// newIdleWatchBody arms the watchdog when idle > 0; idle == 0 returns the
// body unchanged (watchdog disabled).
func newIdleWatchBody(rc io.ReadCloser, idle time.Duration) io.ReadCloser {
	if idle <= 0 {
		return rc
	}
	w := &idleWatchBody{
		rc:   rc,
		idle: idle,
		// UNBUFFERED handoff: the pump can only be one read ahead of the
		// consumer, so a terminal body error (io.EOF) is only queued AFTER
		// the previous chunk was received — chunk and EOF can never be
		// simultaneously ready in Read's select. A buffered channel here
		// raced exactly that way: select could serve the EOF and silently
		// drop the final chunk (truncated stream, lost message_stop).
		ch:       make(chan idleRead),
		errc:     make(chan error, 1),
		fired:    make(chan struct{}),
		pumpDone: make(chan struct{}),
		stopped:  make(chan struct{}),
	}
	go w.pump()
	return w
}

// pump performs the blocking reads off-body; each chunk or terminal error
// is handed to the single consumer. Exits when the body errors, when the
// watchdog fires (the fire closes the body, unblocking the read), or when
// the wrapper is torn down.
func (w *idleWatchBody) pump() {
	defer close(w.pumpDone)
	buf := make([]byte, 64*1024)
	for {
		n, err := w.rc.Read(buf)
		select {
		case <-w.stopped:
			return
		default:
		}
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			select {
			case w.ch <- idleRead{buf: chunk}:
			case <-w.stopped:
				return
			}
		}
		if err != nil {
			select {
			case w.errc <- err:
			case <-w.stopped:
			}
			return
		}
	}
}

// arm schedules the idle fire for the current generation.
func (w *idleWatchBody) arm() {
	gen := w.gen.Add(1)
	time.AfterFunc(w.idle, func() {
		if w.gen.Load() == gen {
			w.fire()
		}
	})
}

// fire is the watchdog callback: tear the body down (unblocking the pump)
// and signal the fired channel exactly once.
func (w *idleWatchBody) fire() {
	w.fireOnce.Do(func() {
		close(w.fired)
		_ = w.rc.Close()
	})
}

// Read yields one pumped chunk, re-arming the idle timer per delivery.
// The FIRST delivery also arms the watchdog — pre-first-byte silence (the
// prefill) never trips it.
func (w *idleWatchBody) Read(p []byte) (int, error) {
	if len(w.pend) > 0 {
		// Leftover from a previous short read: drain it first (no new
		// chunk delivery, no re-arm — the timer for the original chunk
		// still governs).
		n := copy(p, w.pend)
		w.pend = w.pend[n:]
		return n, nil
	}
	select {
	case r := <-w.ch:
		w.arm()
		n := copy(p, r.buf)
		if n < len(r.buf) {
			w.pend = r.buf[n:] // short read: keep the tail for the next call
		}
		return n, nil
	case err := <-w.errc:
		// The watchdog's fire CLOSES the body, which makes the pump's
		// in-flight Read return an error — so when both are ready, the
		// idle signal is the root cause and must win over the raw
		// close-induced error.
		select {
		case <-w.fired:
			return 0, ErrStreamIdle
		default:
		}
		return 0, err // io.EOF passes through unchanged
	case <-w.fired:
		return 0, ErrStreamIdle
	case <-w.stopped:
		return 0, io.ErrClosedPipe
	}
}

// stop tears down the pump and the body (Close/terminal path).
func (w *idleWatchBody) stop() {
	w.stopOnce.Do(func() {
		close(w.stopped)
		w.gen.Add(1) // invalidate any outstanding timer
		_ = w.rc.Close()
	})
}

// Close stops the watchdog and closes the underlying body.
func (w *idleWatchBody) Close() error {
	w.stop()
	<-w.pumpDone
	return nil
}

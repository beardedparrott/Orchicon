package orchicon

import (
	"bufio"
	"io"
	"strings"
)

// SSEDecoded is one decoded SSE frame.
//
// Both the Anthropic Messages and OpenAI Chat Completions streams (and the
// legacy Command Code envelope stream) carry JSON payloads on "data:" lines;
// Anthropic additionally tags frames with "event:" lines. [DONE] is the
// OpenAI sentinel terminating the stream.
type SSEDecoded struct {
	Event string // value of the "event:" line, "" when absent (OpenAI style)
	Data  string // value of the concatenated "data:" lines, "" for comment-only frames
}

// Done reports whether this frame is the OpenAI [DONE] sentinel.
func (f SSEDecoded) Done() bool { return f.Data == "[DONE]" }

// Empty reports whether the frame carries nothing decodable.
func (f SSEDecoded) Empty() bool { return f.Event == "" && f.Data == "" }

// sseReader is the shared minimal SSE reader: a bufio scanner over the
// response body splitting on blank lines, accumulating "data:" lines and
// the latest "event:" line per frame. Comment (":") and unknown-field lines
// are ignored per the SSE spec. Both wire clients and the legacy decoder
// consume this reader.
type sseReader struct {
	sc *bufio.Scanner
}

func newSSEReader(r io.Reader) *sseReader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // up to 16 MiB per frame
	return &sseReader{sc: sc}
}

// Next returns the next frame; ok is false at clean EOF. A scanner error
// (including a mid-stream disconnect) surfaces as the error return.
func (r *sseReader) Next() (SSEDecoded, bool, error) {
	var (
		frame  SSEDecoded
		gotAny bool
	)
	for r.sc.Scan() {
		line := r.sc.Text()
		switch {
		case line == "":
			if gotAny {
				return frame, true, nil
			}
			// consecutive blank lines between frames — keep scanning
		case strings.HasPrefix(line, ":"):
			// comment/heartbeat — ignored per the SSE spec
		case strings.HasPrefix(line, "data:"):
			gotAny = true
			v := strings.TrimPrefix(line, "data:")
			if strings.HasPrefix(v, " ") {
				v = v[1:]
			}
			if frame.Data == "" {
				frame.Data = v
			} else {
				frame.Data += "\n" + v
			}
		case strings.HasPrefix(line, "event:"):
			gotAny = true
			v := strings.TrimPrefix(line, "event:")
			if strings.HasPrefix(v, " ") {
				v = v[1:]
			}
			frame.Event = v
		default:
			// Unknown field lines (id:, retry:, binary) — ignored.
		}
	}
	if err := r.sc.Err(); err != nil {
		return SSEDecoded{}, false, err
	}
	if gotAny {
		// EOF flushed a final unterminated frame (server dropped without the
		// trailing blank line) — deliver it; the wire clients treat a frame
		// missing its event type / payload as a clean-failure signal.
		return frame, true, nil
	}
	return SSEDecoded{}, false, nil
}

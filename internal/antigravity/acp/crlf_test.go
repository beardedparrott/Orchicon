package acp

// crlf_test.go — AC3: a CRLF-terminated line decodes IDENTICALLY to an LF-terminated one.
//
// WHY THIS IS A TEST AND NOT A COMPILE CHECK. cross-vet proves this package builds for Windows; it
// cannot prove what happens when agy on Windows writes `\r\n`. Two separate readers touch the wire,
// and on the Linux CI box neither would ever see a carriage return:
//
//  1. the LOCAL transport (pumpLines, bufio.ReadLine) — ReadLine already drops the terminator;
//  2. the CONTAINER transport (trimTrailingNewline), which strips `\n` and, before this change, left
//     the `\r` glued to the JSON object.
//
// A trailing `\r` after the closing brace is tolerated by encoding/json as whitespace, so the damage
// is NOT a parse error — it is silent. The case that actually bites is a `\r` INSIDE a string value,
// which is what agy's own headless sample writes (`"output":"hello_headless_demo\r\n"`, i.e. an
// ESCAPED backslash-r inside the JSON). That must survive intact while only the LINE TERMINATOR is
// removed, so the test pins both halves: strip the terminator, never touch the payload.

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
)

// crlfTestMessages is a three-message slice of a real turn: the `initialize` request, its result, and
// a session update carrying an ESCAPED `\r\n` inside a string value (the shape agy's headless doc
// documents for tool output on Windows).
var crlfTestMessages = []string{
	`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`,
	`{"jsonrpc":"2.0","result":{"protocolVersion":1,"agentInfo":{"name":"Antigravity ACP Server"}},"id":1}`,
	`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s-1","update":{"sessionUpdate":"tool_call_update","content":[{"type":"content","content":{"type":"text","text":"hello_headless_demo\r\n"}}]}}}`,
}

// drainLines runs the real line pump over r and returns every line it produced.
func drainLines(t *testing.T, r io.Reader) [][]byte {
	t.Helper()
	ch := make(chan []byte, 16)
	go pumpLines(r, ch, func() { t.Error("unexpected over-long line") })
	var out [][]byte
	for line := range ch {
		out = append(out, line)
	}
	return out
}

// TestPumpLinesCRLFMatchesLF is AC3's core: the SAME payload, once LF-terminated and once
// CRLF-terminated, must produce byte-identical lines and decode into equal messages.
func TestPumpLinesCRLFMatchesLF(t *testing.T) {
	lf := strings.Join(crlfTestMessages, "\n") + "\n"
	crlf := strings.Join(crlfTestMessages, "\r\n") + "\r\n"

	lfLines := drainLines(t, strings.NewReader(lf))
	crlfLines := drainLines(t, strings.NewReader(crlf))

	if len(lfLines) != len(crlfTestMessages) {
		t.Fatalf("LF input produced %d lines, want %d: %q", len(lfLines), len(crlfTestMessages), lfLines)
	}
	if len(crlfLines) != len(crlfTestMessages) {
		t.Fatalf("CRLF input produced %d lines, want %d: %q", len(crlfLines), len(crlfTestMessages), crlfLines)
	}
	for i := range lfLines {
		if !bytes.Equal(lfLines[i], crlfLines[i]) {
			t.Errorf("line %d: CRLF form %q != LF form %q — the carriage return must not reach the decoder",
				i, crlfLines[i], lfLines[i])
			continue
		}
		if bytes.ContainsAny(lfLines[i], "\r\n") {
			t.Errorf("line %d still carries a line terminator: %q", i, lfLines[i])
		}
		// Identical decode, which is what the transport actually promises its callers.
		var a, b rpcMessage
		if err := json.Unmarshal(lfLines[i], &a); err != nil {
			t.Fatalf("line %d does not decode: %v (%q)", i, err, lfLines[i])
		}
		if err := json.Unmarshal(crlfLines[i], &b); err != nil {
			t.Fatalf("line %d (CRLF) does not decode: %v (%q)", i, err, crlfLines[i])
		}
		if !reflect.DeepEqual(a, b) {
			t.Errorf("line %d decoded differently:\n LF   = %+v\n CRLF = %+v", i, a, b)
		}
	}

	// THE PAYLOAD IS UNTOUCHED: the escaped `\r\n` INSIDE the JSON string survives, because only the
	// line terminator is stripped. Losing it would silently corrupt every Windows tool output.
	last := crlfLines[len(crlfLines)-1]
	if !bytes.Contains(last, []byte(`hello_headless_demo\r\n`)) {
		t.Errorf("the escaped \\r\\n inside the JSON payload was damaged: %q", last)
	}
	var msg rpcMessage
	if err := json.Unmarshal(last, &msg); err != nil {
		t.Fatalf("decode the tool-output update: %v", err)
	}
	var p struct {
		Update struct {
			Content []struct {
				Content struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"content"`
		} `json:"update"`
	}
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		t.Fatalf("decode the update envelope: %v", err)
	}
	if got, want := p.Update.Content[0].Content.Text, "hello_headless_demo\r\n"; got != want {
		t.Errorf("the tool output text decodes to %q, want %q", got, want)
	}
}

// TestPumpLinesSingleTrailingLineWithoutTerminator covers the EOF arm: a final message that carries
// no terminator at all must still be delivered, so a crashed child's last line is not lost.
func TestPumpLinesSingleTrailingLineWithoutTerminator(t *testing.T) {
	ch := make(chan []byte, 4)
	go pumpLines(strings.NewReader(crlfTestMessages[0]), ch, nil)
	var got [][]byte
	for l := range ch {
		got = append(got, l)
	}
	if len(got) != 1 || string(got[0]) != crlfTestMessages[0] {
		t.Fatalf("a terminator-less final line produced %q, want exactly %q", got, crlfTestMessages[0])
	}
}

// TestPumpLinesDropsOverLongLine is the over-long guard: the callback fires and the PUMP SURVIVES, so
// one giant tool output does not end the session. It also proves the free function kept the
// behaviour the method had before the refactor.
func TestPumpLinesDropsOverLongLine(t *testing.T) {
	huge := strings.Repeat("x", maxLineBytes+16)
	in := huge + "\r\n" + crlfTestMessages[0] + "\r\n"
	ch := make(chan []byte, 4)
	drops := 0
	go pumpLines(strings.NewReader(in), ch, func() { drops++ })
	var got [][]byte
	for l := range ch {
		got = append(got, l)
	}
	if drops != 1 {
		t.Errorf("over-long line reported %d drops, want 1", drops)
	}
	if len(got) != 1 || string(got[0]) != crlfTestMessages[0] {
		t.Fatalf("after dropping the over-long line the pump produced %q, want exactly %q", got, crlfTestMessages[0])
	}
}

// TestTrimTrailingNewline is AC3 for the CONTAINER transport: it is the function that actually had
// the bug (it stripped `\n` only, so `\r` survived into the decoder).
func TestTrimTrailingNewline(t *testing.T) {
	cases := []struct{ in, want string }{
		{"{\"a\":1}\n", "{\"a\":1}"},
		{"{\"a\":1}\r\n", "{\"a\":1}"}, // THE WINDOWS CASE — the regression this change fixes
		{"{\"a\":1}\r", "{\"a\":1}"},   // a chunk boundary that split CRLF
		{"{\"a\":1}", "{\"a\":1}"},     // no terminator at all
		{"", ""},
		// A `\r` that is part of the CONTENT must not be stripped when it is not a terminator.
		{"{\"text\":\"a\\r\\nb\"}\r\n", "{\"text\":\"a\\r\\nb\"}"},
	}
	for _, c := range cases {
		if got := trimTrailingNewline(c.in); got != c.want {
			t.Errorf("trimTrailingNewline(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestContainerFrameCRLFDecodes is AC3 end to end for the container path: the chunk trim plus the
// wire's own trimming must leave a line the decoder accepts, in both terminator styles.
func TestContainerFrameCRLFDecodes(t *testing.T) {
	for _, tc := range []struct{ name, term string }{
		{"LF", "\n"},
		{"CRLF", "\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The container transport's output, as the daemon delivers it: one chunk per line,
			// terminator included.
			for _, msg := range crlfTestMessages {
				line := []byte(trimTrailingNewline(msg + tc.term))
				// conn.read()'s own guard, unchanged, must then be a no-op on the message itself.
				line = bytes.TrimSpace(line)
				var m rpcMessage
				if err := json.Unmarshal(line, &m); err != nil {
					t.Fatalf("a %s-terminated frame did not decode: %v (%q)", tc.name, err, line)
				}
				if m.JSONRPC != "2.0" {
					t.Fatalf("decoded jsonrpc = %q, want 2.0", m.JSONRPC)
				}
			}
		})
	}
}

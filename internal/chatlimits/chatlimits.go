// Package chatlimits holds the bound one chat message must satisfy.
//
// ONE OWNER, because the same number has to hold in four places at once — the two Ask Orchicon RPCs, the
// three execution message RPCs, the TUI composer's counter and the web composer's counter — and a client
// that warns at a different number than the server rejects at is worse than no warning at all.
package chatlimits

import (
	"fmt"
	"unicode/utf8"

	"connectrpc.com/connect"
)

// MaxMessageChars bounds one chat message.
//
// WHY IT MOVED. The limit was 10,000 and it was too small for the thing people actually do with a composer:
// a real prompt with a pasted stack trace, a log, or a code block is ordinary input to an assistant whose job
// is reading code. At 10,000 the operator discovers the cap mid-paste, after the text is already in the box.
//
// THE CAP ITSELF IS NOT THE PROBLEM, so it stays: unbounded input is a memory and cost hazard, not a feature.
// It now sits far enough above normal use that reaching it is deliberate rather than accidental.
const MaxMessageChars = 100_000

// WarnAt is where a client starts SHOWING the character counter.
//
// A counter under every sentence is noise, so it appears only where the limit becomes relevant — the last
// fifth. If the number changes, this moves with it, and no client can drift because they all read it here.
const WarnAt = MaxMessageChars * 4 / 5

// TooLong returns the RPC error for an over-long message, or nil when msg fits.
//
// THE ERROR IS BUILT HERE so the five enforcement sites cannot word it differently, and so it can name the
// LENGTH AS WELL AS THE BOUND: "max N characters" left the operator to count for themselves, which is the
// least useful thing to say to someone who has just pasted a large block.
func TooLong(msg string) error {
	n := utf8.RuneCountInString(msg)
	if n <= MaxMessageChars {
		return nil
	}
	return connect.NewError(connect.CodeInvalidArgument,
		fmt.Errorf("message too long: %d characters, and the limit is %d", n, MaxMessageChars))
}

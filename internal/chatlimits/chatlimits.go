// Package chatlimits holds the bound one chat message must satisfy.
package chatlimits

import (
	"fmt"
	"unicode/utf8"

	"connectrpc.com/connect"
)

// MaxMessageChars bounds one chat message.
const MaxMessageChars = 100_000

// WarnAt is where a client starts SHOWING the character counter.
const WarnAt = MaxMessageChars * 4 / 5

// TooLong returns the RPC error for an over-long message, or nil when msg fits.
func TooLong(msg string) error {
	n := utf8.RuneCountInString(msg)
	if n <= MaxMessageChars {
		return nil
	}
	return connect.NewError(connect.CodeInvalidArgument,
		fmt.Errorf("message too long: %d characters, and the limit is %d", n, MaxMessageChars))
}

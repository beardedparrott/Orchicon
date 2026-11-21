package mcpsettings

// key_grammar_test.go — env keys and HTTP header names are DIFFERENT grammars.
//
// THE BUG THIS PINS. Both maps were validated by one regex, the environment-variable grammar
// ^[A-Za-z_][A-Za-z0-9_]*$, so every hyphenated header was rejected as invalid. That is not a corner
// case: X-Api-Key, X-Request-Id and X-Goog-Api-Key are the ordinary shape for the streamable-HTTP
// transport these settings describe, so most real remote MCP servers could not be configured at all.
//
// The failure surfaced as a red test in CI (TestMCPEmbeddedSecretRefExpansionCases, which used the
// header name "X-Pair"), and had been red on develop since 2026-11-14.
//
// These tests run WITHOUT a database: the grammar is a pure function, so there is no reason for the
// guard against this regression to be gated behind ORCHICON_TEST_DSN like the test that found it.

import (
	"strings"
	"testing"
)

func TestHeaderKeysAcceptRealHTTPHeaderNames(t *testing.T) {
	// Every one of these is a legal HTTP header name and was REJECTED before the fix.
	accepted := []string{
		"Authorization", // the one shape that used to work
		"X-Pair",
		"X-Api-Key",
		"X-Request-Id",
		"X-Goog-Api-Key",
		"Content-Type",
		"Accept",
		"X_Custom",   // underscore is legal in a token, and env-style names must keep working
		"X-Trace.V1", // '.' is a token character
		"X-Weird~+^`|",
	}
	for _, k := range accepted {
		if !headerKeyRE.MatchString(k) {
			t.Errorf("header key %q rejected by %s, but it is a legal HTTP header name",
				k, headerKeyPattern)
		}
	}
}

func TestHeaderKeysRejectNamesThatAreNotTokens(t *testing.T) {
	// A rejected key must not be a way to smuggle a second header or a request line through one map
	// entry, so colon and whitespace must never match.
	rejected := []string{
		"",
		"X Bad",          // space
		"X-Pair:value",   // colon would inject a second header
		"X-Pair\r\nEvil", // CRLF injection
		"X-Pair\nEvil",
		"X-Pair\tvalue",
		"X-Pair/Path", // '/' is a separator, not a token char
		"X-Pair()",    // delimiters
	}
	for _, k := range rejected {
		if headerKeyRE.MatchString(k) {
			t.Errorf("header key %q ACCEPTED by %s — it must be rejected (injection risk)",
				k, headerKeyPattern)
		}
	}
}

func TestEnvKeysStayStrict(t *testing.T) {
	// The env grammar must NOT be relaxed alongside the header grammar: a POSIX environment name cannot
	// contain a hyphen, so a hyphenated "env key" is a caller mistake worth reporting.
	accepted := []string{"PATH", "A_TOKEN", "_PRIVATE", "x9"}
	for _, k := range accepted {
		if !envKeyRE.MatchString(k) {
			t.Errorf("env key %q rejected by %s, but it is a legal environment name", k, envKeyPattern)
		}
	}
	rejected := []string{"X-Pair", "X-Api-Key", "1LEADING_DIGIT", "", "HAS SPACE", "HAS:COLON"}
	for _, k := range rejected {
		if envKeyRE.MatchString(k) {
			t.Errorf("env key %q ACCEPTED by %s, but an environment name cannot look like that",
				k, envKeyPattern)
		}
	}
}

// TestValidateMapKeysReportsTheGrammarItUsed matters because the error text is what an operator sees.
// If headers were validated by the env rule again, the message would quote envKeyPattern and silently
// mislead whoever is trying to configure a server.
func TestValidateMapKeysReportsTheGrammarItUsed(t *testing.T) {
	err := validateMapKeys(map[string]string{"X Bad": "v"}, "headers", 32, headerKeys)
	if err == nil {
		t.Fatal("validateMapKeys accepted a header key containing a space")
	}
	if got := err.Error(); !strings.Contains(got, headerKeyPattern) {
		t.Errorf("error %q does not quote the HEADER grammar %s", got, headerKeyPattern)
	}

	// And the env side must quote the env rule, not the header one.
	err = validateMapKeys(map[string]string{"X-Pair": "v"}, "env", 64, envKeys)
	if err == nil {
		t.Fatal("validateMapKeys accepted a hyphenated ENV key")
	}
	if got := err.Error(); !strings.Contains(got, envKeyPattern) {
		t.Errorf("error %q does not quote the ENV grammar %s", got, envKeyPattern)
	}
}

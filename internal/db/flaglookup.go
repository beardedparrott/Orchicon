package db

// flaglookup.go — the one indirection that lets the prod guard tell a test binary from a server.
//
// It is a variable rather than a direct flag.Lookup call so the guard's OWN test can exercise both sides of the
// decision. A guard that can only be tested in whichever mode the test binary happens to be is a guard with an
// untested branch, and the branch that matters here is precisely "is this a test binary" — the whole refusal
// hangs off it.
//
// The production value is flag.Lookup and nothing more; nothing in the server path ever replaces it.
import "flag"

var flagLookup = flag.Lookup

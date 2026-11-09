package db

// flaglookup.go — the one indirection that lets the prod guard tell a test binary from a server.
import "flag"

var flagLookup = flag.Lookup

package backup

// pgclient.go — HOW this process reaches Postgres with pg_dump/psql.
//
// Backups shell out to pg_dump/psql, and the plane is a HOST process that owns
// its Postgres as a HOST process too (one shape: the plane and its services are
// host processes, always). So the client is the LOCAL one, resolved in two steps:
//
//  1. the instance's OWN resolved record (<DataDir>/infra-binaries.json, written by
//     cmd/orchicon/binaries.go), because the PostgreSQL distribution the supervisor
//     runs is the one whose client version matches its server exactly, and on a
//     downloaded install it is on no PATH at all;
//  2. else the LOCAL pg_dump/psql on PATH, which is correct for a host install that
//     has postgresql-client of its own.
//
// WHAT IS NOT HERE ANY MORE: the `docker exec` client. It existed for the retired
// single-container deployment, where Postgres lived in a container the plane had to
// reach through the engine CLI. That shape is gone, so a container client would have
// been a path that can only fail — and it failed loudly: with a container name left
// in the environment the plane reported `docker ... is required to run pg_dump
// inside container ...` while a perfectly good client sat on the host.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	// dataDirEnv is the control plane's state dir (internal/config, and the same
	// variable internal/backup/backup.go's DefaultDir reads). The infra-binaries
	// record written by cmd/orchicon/binaries.go lives directly under it.
	dataDirEnv = "ORCHICON_DATA_DIR"

	// pgToolDump / pgToolRestore are the two tools this package drives.
	pgToolDump    = "pg_dump"
	pgToolRestore = "psql"
)

// pgClient is a resolved way to run a Postgres client tool.
type pgClient struct {
	// dsn is the DSN as the tool must see it — the host's view, because the tool
	// runs on this host.
	dsn string
}

// execPath resolves the executable a command will actually run, so a missing
// tool is reported here — with guidance — rather than surfacing as a bare
// `exec: "pg_dump": executable file not found in $PATH` from the deep end.
func (c pgClient) execPath(tool string) (string, error) {
	// FIRST the instance's OWN resolved record — the distribution the supervisor
	// runs is the one whose client version matches its server exactly, and on a
	// downloaded install it is on no PATH at all. See resolvedInfraBinary.
	if p := infraRecordLookup(tool); p != "" {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	path, err := exec.LookPath(tool)
	if err != nil {
		return "", fmt.Errorf("%s is not on this PATH and the instance's own Postgres distribution does not "+
			"carry it — install postgresql-client, or point $%s at the instance's data dir so its own %s "+
			"(recorded in %s) can be used: %w", tool, dataDirEnv, tool, infraRecordFile, err)
	}
	return path, nil
}

// command builds the command for tool with the given flags.
func (c pgClient) command(ctx context.Context, tool string, args ...string) (*exec.Cmd, error) {
	exe, err := c.execPath(tool)
	if err != nil {
		return nil, err
	}
	return exec.CommandContext(ctx, exe, args...), nil
}

// infraRecordLookup is the seam through which the client prefers the PostgreSQL
// distribution the resolver already chose, over whatever PATH happens to hold. It is
// a var so a test can point it at a fixture record without a data dir.
var infraRecordLookup = resolvedInfraBinary

// infraRecordFile is the record cmd/orchicon/binaries.go writes ONCE per instance
// ("ONE RESOLVER, ONE RECORD"): <DataDir>/infra-binaries.json, keyed by field name.
const infraRecordFile = "infra-binaries.json"

// resolvedInfraBinary returns the absolute path of a resolved PostgreSQL tool from
// the instance's own record, or "" when there is no record or no such field.
//
// WHY THE RECORD AND NOT PATH. On a host whose Postgres came from the DOWNLOAD path
// the distribution is unpacked at <DataDir>/bin/postgres/bin, which is on NOBODY's
// PATH — so `exec.LookPath("pg_dump")` fails on the very install the resolver just
// provisioned. Reading the record is not a second resolution path: it is the SAME
// answer the supervisor that runs the server used, which is the whole reason the
// record exists. PATH stays as the fallback for a host with its own
// postgresql-client.
//
// A malformed or unreadable record is NOT an error here — it degrades to the PATH
// lookup and its existing actionable message, because a broken record must not be
// able to make backup impossible on a host where a working client is on PATH.
func resolvedInfraBinary(tool string) string {
	dataDir := strings.TrimSpace(os.Getenv(dataDirEnv))
	if dataDir == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(dataDir, infraRecordFile))
	if err != nil {
		return ""
	}
	var rec map[string]any
	if err := json.Unmarshal(b, &rec); err != nil {
		return ""
	}
	// The keys are the record's JSON field names, which are the tool names for the
	// tools this package drives (pg_dump, psql).
	v, _ := rec[tool].(string)
	return strings.TrimSpace(v)
}

// resolvePGClient decides how to reach the database named by dsn. There is exactly
// one way now — the local client — so this is deliberately trivial; it stays a
// function because Create and Restore must share the SAME answer (a restore can
// never reach a different Postgres than the dump came from).
func resolvePGClient(dsn string) pgClient {
	return pgClient{dsn: dsn}
}

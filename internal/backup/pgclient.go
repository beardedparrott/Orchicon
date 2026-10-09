package backup

// pgclient.go — HOW this process reaches Postgres with pg_dump/psql.
//
// WHY THIS IS NOT A CONSTANT. Backup and restore shell out to pg_dump/psql, and
// f7e2fa7d ("single-container deployment") changed those invocations to run the
// binaries LOCALLY, on the assumption that the plane always shares a process
// namespace with Postgres ("there is no docker-exec indirection"). That premise
// stopped being true the moment HOST residency became the default shape, and the
// operator hit it:
//
//	Error: [internal] backup: pg_dump: exec: "pg_dump": executable file not found in $PATH
//
// In host residency the plane is a HOST process (no pg_dump on the host — the
// host has docker, not postgresql-client) and Postgres runs inside the instance's
// services container, publishing only a loopback port (dev 5432, prod 5433). The
// binaries that can reach that database exist INSIDE the container. So the
// resolver below picks the client the RESIDENCY calls for:
//
//  1. an explicitly named container (ORCHICON_PG_CONTAINER) — the launcher's and
//     the installer's way of saying exactly where Postgres is;
//  2. else, the INSTANCE'S OWN SERVICES CONTAINER, discovered and VERIFIED (see
//     discoverInstanceContainer) when the DSN is loopback and that container
//     publishes the DSN's port — this is what makes a hand-typed
//     `orchicon db backup` work on a host-residency install without the operator
//     having to export anything;
//  3. else the LOCAL pg_dump/psql, which is correct for a container-resident
//     plane (the binaries are in the image; the image has no docker CLI) and for
//     a plain host install pointed at its own Postgres.
//
// PREFERRING THE CONTAINER OVER A LOCAL BINARY IS DELIBERATE even when a local
// one exists: a host with an older postgresql-client installed would otherwise
// fail with "server version mismatch; aborting" against the container's newer
// server, which reads as a broken backup rather than as a version skew. The
// container's own tools always match its own server exactly.

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
)

const (
	// pgContainerEnv names the container that carries Postgres, when the caller
	// knows it. Set by scripts/container.sh (plane_env) and
	// cmd/orchicon/install_host.go (planeEnv) for a HOST-resident plane; unset for
	// every other residency, where the local binaries are correct.
	pgContainerEnv = "ORCHICON_PG_CONTAINER"

	// instanceContainerPrefix is the container-name convention shared by the two
	// launchers (scripts/container.sh: NAME="orchicon-cnt-dev", and
	// cmd/orchicon/install.go: name := "orchicon-cnt-" + instance). It is only the
	// FALLBACK path — discovery VERIFIES the port mapping before trusting the name
	// (see discoverInstanceContainer), so a wrong guess degrades to "use a local
	// client", never to a dump of the wrong database.
	instanceContainerPrefix = "orchicon-cnt-"

	// containerPGPort is the port Postgres listens on INSIDE the services container
	// (cmd/orchicon/container.go postgresProc: `postgres -D … -p 5432`). The
	// host-plane DSN carries the PUBLISHED port instead, which is why the container
	// client rewrites it: dev publishes 5432:5432, prod 5433:5432.
	containerPGPort = "5432"

	// pgToolDump / pgToolRestore are the two tools this package drives.
	pgToolDump    = "pg_dump"
	pgToolRestore = "psql"
)

// pgClient is a resolved way to run a Postgres client tool.
type pgClient struct {
	// dsn is the DSN AS THE CHOSEN TOOL MUST SEE IT — the host's view for a local
	// client, the container's own view (port rewritten to containerPGPort) for a
	// container client.
	dsn string
	// container is the container to exec into; empty means a LOCAL client.
	container string
	// dockerBin is the docker CLI to exec with (only meaningful with container set).
	dockerBin string
}

// local reports whether this client runs the tool directly on this host.
func (c pgClient) local() bool { return c.container == "" }

// execPath resolves the executable a command will actually run, so a missing
// tool is reported here — with guidance — rather than surfacing as a bare
// `exec: "pg_dump": executable file not found in $PATH` from the deep end.
func (c pgClient) execPath(tool string) (string, error) {
	if c.local() {
		path, err := exec.LookPath(tool)
		if err != nil {
			return "", fmt.Errorf("%s is not on this PATH and no Postgres container was reachable to run it in — "+
				"set %s=<container> to name the instance's services container, or install postgresql-client "+
				"(the container's own %s always matches its server version)", tool, pgContainerEnv, tool)
		}
		return path, nil
	}
	path, err := exec.LookPath(c.dockerBin)
	if err != nil {
		return "", fmt.Errorf("docker (%s) is required to run %s inside container %s, but it is not on this PATH",
			c.dockerBin, tool, c.container)
	}
	return path, nil
}

// command builds the command for tool with the given flags. argv is EXACTLY the
// same in both shapes — pg_dump/psql receive the DSN and their flags — and only
// the transport differs, so a flag can never be honoured in one residency and
// silently dropped in the other.
func (c pgClient) command(ctx context.Context, tool string, args ...string) (*exec.Cmd, error) {
	exe, err := c.execPath(tool)
	if err != nil {
		return nil, err
	}
	if c.local() {
		return exec.CommandContext(ctx, exe, args...), nil
	}
	// -i is REQUIRED, not incidental: restore pipes the dump into psql's stdin.
	return exec.CommandContext(ctx, exe, append([]string{"exec", "-i", c.container, tool}, args...)...), nil
}

// pgProber is the host-probing seam, so resolution is testable without docker.
type pgProber interface {
	// lookPath resolves an executable on PATH.
	lookPath(name string) (string, error)
	// output runs a probe command and returns its trimmed stdout.
	output(name string, args ...string) (string, error)
}

// realProber probes the real host.
type realProber struct{}

func (realProber) lookPath(name string) (string, error) { return exec.LookPath(name) }

func (realProber) output(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	return strings.TrimSpace(string(out)), err
}

// probe is the package-level seam (swapped by tests).
var probe pgProber = realProber{}

// resolvePGClient decides how to reach the database named by dsn.
//
// It returns an error ONLY when a container was selected AND the DSN cannot be
// rewritten for it — a container client with the host's published port in the
// DSN would connect to nothing inside the container, and a comment cannot fix
// that, so it is refused out loud.
func resolvePGClient(dsn string) (pgClient, error) {
	if name := strings.TrimSpace(os.Getenv(pgContainerEnv)); name != "" {
		return containerClient(name, dsn)
	}
	if name, ok := discoverInstanceContainer(dsn); ok {
		return containerClient(name, dsn)
	}
	return pgClient{dsn: dsn}, nil
}

// containerClient builds the exec-into-container client, translating the DSN to
// the container's own view of Postgres.
func containerClient(name, dsn string) (pgClient, error) {
	inner, err := dsnForContainer(dsn)
	if err != nil {
		return pgClient{}, err
	}
	return pgClient{dsn: inner, container: name, dockerBin: "docker"}, nil
}

// dsnForContainer rewrites a DSN's port to containerPGPort so the tool run
// inside the container connects to the Postgres that is actually there.
func dsnForContainer(dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("cannot reach Postgres in a container: the DSN %q is not a URL — "+
			"use postgres://user:pass@host:port/db (or unset %s to use a local client)", dsn, pgContainerEnv)
	}
	u.Host = net.JoinHostPort(u.Hostname(), containerPGPort)
	return u.String(), nil
}

// discoverInstanceContainer finds the services container that is publishing the
// loopback port this DSN points at, and VERIFIES it before it is trusted:
//
//	loopback DSN host              → only a local services container can be it
//	+ container running            → a stopped container cannot be exec'd into
//	+ its :5432 mapping == our port → the mapping IS our port, or we would be
//	                                  dumping a DIFFERENT instance's database
//
// Every check must pass; any failure falls through to the local client, which is
// the pre-existing behaviour rather than a guess.
func discoverInstanceContainer(dsn string) (string, bool) {
	if _, err := probe.lookPath("docker"); err != nil {
		return "", false // no docker CLI (a container-resident plane) → local client
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Host == "" || !isLoopbackHost(u.Hostname()) {
		return "", false
	}
	port := u.Port()
	if port == "" {
		port = containerPGPort
	}
	name := instanceContainerPrefix + envOr("ORCHICON_INSTANCE", "dev")

	if out, err := probe.output("docker", "inspect", "--format", "{{.State.Running}}", name); err != nil || out != "true" {
		return "", false
	}
	// docker port prints one "<ip>:<hostport>" per mapping ("127.0.0.1:5433", or
	// ":::5433" for a v6 wildcard); the LAST colon-field is the host port.
	out, err := probe.output("docker", "port", name, containerPGPort)
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if i := strings.LastIndex(line, ":"); i >= 0 && line[i+1:] == port {
			return name, true
		}
	}
	return "", false
}

// isLoopbackHost reports whether a DSN host names this machine's loopback.
func isLoopbackHost(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "::1":
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// envOr returns the environment value or a fallback.
func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

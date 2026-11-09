package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/config"
	"github.com/beardedparrott/orchicon/internal/containerengine"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/runtime"
)

// daemonPool lazily opens (and memoises) the runtime daemon's database handle.
var (
	daemonPoolOnce sync.Once
	daemonPoolInst *db.Pool
	daemonPoolErr  error
	daemonCfg      config.Config
)

// daemonDataDir is the state dir this process reports as its data dir, resolved EAGERLY. IT MUST
// NOT COME FROM daemonCfg.
func daemonDataDir() string { return config.Default().DataDir }

func daemonPool(ctx context.Context, log *slog.Logger) (*db.Pool, error) {
	daemonPoolOnce.Do(func() {
		daemonCfg = config.Default()
		// THE DSN MUST BE THE PLANE'S, NOT THE DEFAULT ONE.
		daemonCfg.PostgresDSN = instancePostgresDSN(daemonCfg.PostgresDSN)
		if daemonCfg.PostgresDSN == "" {
			daemonPoolErr = fmt.Errorf("no postgres DSN configured — cannot read project directories for the mount allowlist")
			return
		}
		daemonPoolInst, daemonPoolErr = db.Open(ctx, daemonCfg.PostgresDSN)
		if daemonPoolErr != nil && log != nil {
			log.Warn("runtime daemon: could not open the database to read project directories; "+
				"mounts are limited to ORCHICON_RUNTIME_ALLOWED_ROOTS until it is reachable",
				"error", daemonPoolErr, "dsn_host", redactDSN(daemonCfg.PostgresDSN))
		}
	})
	return daemonPoolInst, daemonPoolErr
}

// instancePostgresDSN is the DSN for THIS instance's database: the instance's recorded port if
// there is one, otherwise the configured/default value.
func instancePostgresDSN(fallback string) string {
	inst := env("ORCHICON_INSTANCE", "dev")
	ports, err := readResolvedPorts(inst)
	if err != nil {
		return fallback
	}
	port := ports["ORCHICON_POSTGRES_PORT"]
	if port <= 0 {
		return fallback
	}
	return fmt.Sprintf("postgres://orchicon:orchicon@localhost:%d/orchicon?sslmode=disable", port)
}

// redactDSN keeps the host and database of a DSN and drops any credential, so an error context can name the
// database it failed to reach without putting a password in a log file.
func redactDSN(dsn string) string {
	if i := strings.Index(dsn, "://"); i >= 0 {
		rest := dsn[i+3:]
		if at := strings.Index(rest, "@"); at >= 0 {
			return dsn[:i+3] + "***@" + rest[at+1:]
		}
	}
	return dsn
}

// projectDirRoots reads the active projects' directories, for the runtime daemon's mount allowlist.
func projectDirRoots(ctx context.Context, pool *db.Pool, tenantID string, log *slog.Logger) ([]string, error) {
	if pool == nil {
		return nil, nil
	}
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = ttx.Rollback(ctx) }()

	// ONE QUERY, NO PAGINATION, NO STATUS FILTER — and both of those are corrections.
	rows, err := ttx.Tx.Query(ctx, `
		SELECT DISTINCT project_dir
		FROM projects
		WHERE tenant_id = $1
		  AND project_dir IS NOT NULL
		  AND project_dir <> ''`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var dir string
		if err := rows.Scan(&dir); err != nil {
			return nil, err
		}
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		// A RELATIVE project_dir cannot be allowed: it would resolve against whatever working directory the
		// daemon happens to hold, which is not a boundary anyone can reason about.
		if !filepath.IsAbs(dir) {
			if log != nil {
				log.Warn("runtime daemon: ignoring a project with a RELATIVE project_dir — only absolute "+
					"directories can be mount roots", "project_dir", dir)
			}
			continue
		}
		out = append(out, filepath.Clean(dir))
	}
	return out, rows.Err()
}

// runRuntimeDaemon runs the host-side runtime orchestrator (`orchicon runtime-daemon`).
func runRuntimeDaemon(args []string, log *slog.Logger) error {
	hostUID := envInt("ORCHICON_HOST_UID", 1000)
	hostGID := envInt("ORCHICON_HOST_GID", 1000)
	hostHome := os.Getenv("ORCHICON_HOST_HOME")
	if hostHome == "" {
		if h, err := os.UserHomeDir(); err == nil {
			hostHome = h
		}
	}
	allowedRoots := strings.Split(env("ORCHICON_RUNTIME_ALLOWED_ROOTS", filepath.Join(hostHome, "projects")), ",")
	for i := range allowedRoots {
		allowedRoots[i] = strings.TrimSpace(allowedRoots[i])
	}

	socketPath := env("ORCHICON_RUNTIME_SOCKET", filepath.Join(os.TempDir(), "orchicon-runtime", "runtime.sock"))
	if len(args) > 0 {
		socketPath = args[0]
	}

	// Resolve the daemon's own executable once, at process start.
	exePath := ""
	if p, err := os.Executable(); err == nil {
		exePath = p
		if abs, aerr := filepath.Abs(p); aerr == nil {
			exePath = abs
		}
	}

	// Self-copy the daemon binary to a STABLE path next to the socket.
	containerExe, cerr := runtime.ResolveContainerExe(runtime.GOOS(), exePath,
		os.Getenv(runtime.ContainerExeEnv), daemonDataDir())
	if cerr != nil {
		// NOT FATAL HERE, DELIBERATELY: the daemon still serves local-mode work, and a host with no
		// container binary is a supported shape.
		log.Warn("runtime daemon: no linux orchicon binary for runtime containers — container creates will be refused",
			"error", cerr, "env", runtime.ContainerExeEnv)
		containerExe = ""
	}

	if exePath != "" {
		stableExe := filepath.Join(filepath.Dir(socketPath), "orchicon")
		if err := os.MkdirAll(filepath.Dir(stableExe), 0o755); err == nil {
			// COPY THE RIGHT FILE.
			src := exePath
			if containerExe != "" {
				src = containerExe
			}
			if cerr := copySelf(src, stableExe); cerr != nil {
				log.Warn("daemon self-copy failed — runtime containers may break after make clean", "error", cerr,
					"source", src)
			} else {
				// ONLY REPLACE THE MOUNTED BINARY WHEN IT IS THIS HOST'S OWN.
				if containerExe == "" || containerExe == exePath {
					exePath = stableExe
				}
				if containerExe != "" && containerExe != exePath {
					containerExe = stableExe
				}
			}
		} else {
			log.Warn("daemon self-copy dir", "error", err)
		}
	}

	// Resolve the container engine BEFORE the daemon exists: a missing engine is an operator-facing
	// INSTALL problem, so it must fail here with the exact install command (runtime.ResolveEngine's.
	eng, err := runtime.ResolveEngineFromEnv()
	if err != nil {
		return err
	}
	log.Info("runtime engine resolved", "engine", eng.Name, "bin", eng.Bin)

	// THE MACHINE IS THE DAEMON'S BUSINESS TOO, not only the installer's.
	if eng.Name == "podman" {
		// The two Engine types are different packages' views of the same fact; pass the resolved binary across.
		ce := containerengine.Engine{Name: containerengine.EnginePodman, Bin: eng.Bin}
		if ms, merr := containerengine.EnsureMachine(context.Background(), containerengine.DefaultProbe(), ce, os.Stdout); merr != nil {
			log.Warn("runtime daemon: the podman machine is not usable — containers will fail until it is",
				"error", merr, "state", ms)
		} else {
			log.Info("runtime daemon: podman machine ready", "state", ms)
		}
	}

	baseImage := env("ORCHICON_RUNTIME_IMAGE", "ghcr.io/beardedparrott/orchicon-runtime:latest")
	d := &runtime.Daemon{
		SocketPath:    socketPath,
		Engine:        eng,
		Image:         baseImage,
		Images:        runtimeImagesAllowlist(env("ORCHICON_RUNTIME_IMAGES", "")),
		UserID:        hostUID,
		GroupID:       hostGID,
		HostHome:      hostHome,
		AllowedRoots:  allowedRoots,
		// EVERY ACTIVE PROJECT'S project_dir IS MOUNTABLE, read live so a directory set a moment ago works
		// now.
		AllowedRootsSource: func(ctx context.Context) ([]string, error) {
			pool, perr := daemonPool(ctx, log)
			if perr != nil {
				return nil, perr
			}
			return projectDirRoots(ctx, pool, daemonCfg.DeploymentTenantID, log)
		},
		ExePath:       exePath,
		// WHICH FILE THE CONTAINER RUNS — resolved above, never the daemon's own executable on Windows.
		ContainerExe: containerExe,
		// WHERE A DURABLE LINUX BINARY WOULD GO, so a refusal names a path that survives a temp cleaner
		// instead of the %TEMP% self-copy this process mounts from.
		ContainerExeDataDir: daemonDataDir(),
		// ON A HOST WHOSE OWN BINARY THE CONTAINER CANNOT RUN, an empty ContainerExe must REFUSE rather than
		// fall back. Set from the same fact the resolver used, so the two cannot disagree.
		ContainerExeRequired: runtime.GOOS() == "windows" || runtime.GOOS() == "darwin",
		CPUs:          env("ORCHICON_RUNTIME_CPUS", "4"),
		Memory:        env("ORCHICON_RUNTIME_MEMORY", "4g"),
		TmpfsSize:     env("ORCHICON_RUNTIME_TMPFS", "2g"),
		HomeTmpfsSize: env("ORCHICON_RUNTIME_HOME_TMPFS", "2g"),
		// PATH MAPPING (internal/runtime/pathmap.go).
		Paths:         runtime.NewPathMapperFromEnv(runtime.GOOS()),
		MaxAge:        envDur("ORCHICON_RUNTIME_MAX_AGE", 24*time.Hour),
		SweepInterval: envDur("ORCHICON_RUNTIME_SWEEP_INTERVAL", 5*time.Minute),
		Log:           log,
	}

	// PUBLISH THE BASE TAG THE RESOLVE CHAIN SHOULD FALL BACK TO. THIS PROCESS IS THE ONLY ONE THAT
	// KNOWS d.Image, so it is the only one that can repair the fallback.
	db.SetBaseRuntimeImage(baseImage)
	log.Info("runtime base image published to the resolve chain", "base", db.RuntimeBaseImage())

	// Platform-dependent stop signals: SIGINT+SIGTERM on Unix, os.Interrupt on Windows (signals_*.go).
	ctx, stop := signal.NotifyContext(context.Background(), shutdownSignals()...)
	defer stop()
	if err := d.ListenAndServe(ctx); err != nil {
		return err
	}
	return nil
}

// copySelf copies src to dst, refreshing the copy only when src is newer (a daemon rebuild) so a
// stable copy isn't rewritten on every start.
func copySelf(src, dst string) error {
	// CURRENT MEANS IDENTICAL BY CONTENT, NOT BY TIMESTAMP.
	if sameFile(src, dst) {
		return nil
	}
	_ = os.Remove(dst)
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// sameFile reports whether two files have identical content.
func sameFile(a, b string) bool {
	ai, aerr := os.Stat(a)
	bi, berr := os.Stat(b)
	if aerr != nil || berr != nil {
		return false
	}
	if ai.Size() != bi.Size() {
		return false
	}
	af, aerr := os.Open(a)
	if aerr != nil {
		return false
	}
	defer af.Close()
	bf, berr := os.Open(b)
	if berr != nil {
		return false
	}
	defer bf.Close()
	const chunk = 1 << 20
	abuf := make([]byte, chunk)
	bbuf := make([]byte, chunk)
	for {
		an, aerr := io.ReadFull(af, abuf)
		bn, berr := io.ReadFull(bf, bbuf)
		if an != bn || !bytes.Equal(abuf[:an], bbuf[:bn]) {
			return false
		}
		if aerr != nil || berr != nil {
			// EOF on both is the success case; anything else is a short/errored read.
			return (aerr == io.EOF || aerr == io.ErrUnexpectedEOF) &&
				(berr == io.EOF || berr == io.ErrUnexpectedEOF)
		}
	}
}

// runRuntimeSupervisor runs the in-container PID-1 dispatch loop.
func runRuntimeSupervisor(args []string, log *slog.Logger) error {
	socketPath := ""
	if len(args) > 0 {
		socketPath = args[0]
	}
	ensureHomeDirs(log)
	return runtime.RunSupervisor(socketPath, log)
}

// ensureHomeDirs creates the standard $HOME subdirectories the runtime and the tooling it runs
// write into: `.local/share`, `.local/state`, `.config`, `.cache`, `.npm` and `projects`.
func ensureHomeDirs(log *slog.Logger) {
	home := strings.TrimSpace(os.Getenv("HOME"))
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = h
		}
	}
	if home == "" {
		return
	}
	for _, sub := range []string{".local/share", ".local/state", ".config", ".cache", ".npm", "projects"} {
		p := filepath.Join(home, sub)
		if err := os.MkdirAll(p, 0o755); err != nil {
			log.Warn("runtime supervisor: could not create a home directory — the runtime will start "+
				"without it, and tooling writing there may fail", "path", p, "error", err)
		}
	}
}

// runRuntimeClient forwards one request (from stdin) to the supervisor
// socket and relays events to stdout, exiting with the child's code.
func runRuntimeClient(args []string, log *slog.Logger) error {
	socketPath := ""
	if len(args) > 0 {
		socketPath = args[0]
	}
	code, err := runtime.RunClient(socketPath, os.Stdin, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(code)
	return nil
}

// envDur parses a duration env var with a fallback (0 on invalid).
func envDur(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

// splitCSV splits a comma-separated env var into a trimmed slice,
// dropping empties.
func splitCSV(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// runtimeImagesAllowlist resolves the daemon's stock image allowlist.
func runtimeImagesAllowlist(csv string) []string {
	if csv != "" {
		return splitCSV(csv)
	}
	// THE VARIANT TAGS COME FROM runtimeVariantRef, NOT FROM A LOCAL CONCATENATION — and the difference is a
	// reported bug, not tidiness.
	//
	// This built "<repo>:<variant>-<tag>", which for the default tag "latest" produced ":gui-latest" and
	// ":dev-latest". NOTHING PUBLISHES THOSE. release.yml pushes ":gui" and ":dev" (floating) alongside
	// ":gui-<version>" and ":dev-<version>", so the floating tag drops the version instead of becoming "-latest".
	//
	// THE BLAST RADIUS WAS BIGGER THAN THE ALLOWLIST. This value is the daemon's Images list, and the catalogue
	// seeder reconciles each stock row's TAG to whatever the daemon reports — so this line decided the tag the
	// operator's row carried, and a fetch pulled it and failed:
	//
	//	Error: unable to copy from source docker://ghcr.io/beardedparrott/orchicon-runtime:dev-latest:
	//	reading manifest dev-latest in ghcr.io/beardedparrott/orchicon-runtime: manifest unknown
	//
	// It was already fixed in internal/runtimeimage/seed.go and in install.go's runtimeVariantRef; this copy was
	// missed because the rule had THREE homes. Now it has one, and this is a call into it.
	img := env("ORCHICON_RUNTIME_IMAGE", "ghcr.io/beardedparrott/orchicon-runtime:latest")
	// LastIndex, NOT Cut: a repository can carry a PORT, and "localhost:5000/orchicon-runtime:dev" has
	// two colons.
	colon := strings.LastIndex(img, ":")
	if colon <= 0 {
		return nil
	}
	repo := img[:colon]

	// THE VARIANTS ARE LOCAL BUILDS NOW, SO THE ALLOWLIST NAMES THEIR LOCAL TAGS.
	repo = localRepoName(repo)
	return []string{localVariantTag(repo, "gui"), localVariantTag(repo, "dev")}
}

// localVariantTag is the tag a locally-built variant is addressed by: "<repo>:<variant>", a SHORT
// NAME.
func localVariantTag(repo, variant string) string { return repo + ":" + variant }

// localRepoName strips the registry prefix and any owner, leaving the bare repository name:
// "ghcr.io/beardedparrott/orchicon-runtime" -> "orchicon-runtime".
func localRepoName(repo string) string {
	repo = strings.TrimSpace(repo)
	if i := strings.LastIndexByte(repo, '/'); i >= 0 {
		repo = repo[i+1:]
	}
	if repo == "" {
		return "orchicon-runtime"
	}
	return repo
}

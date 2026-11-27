package antigravity

// spawn.go — THE LAUNCH PRELUDE, shared by the two populations this adapter drives.
//
// THERE ARE TWO: worker executions (session.go / sessionrun.go) and, since child 6, Ask conversations
// (ask.go). Both launch the SAME child the SAME way — the tenant's auth method, the key-free argv, the
// OS-level guard shim on PATH, and the bypass refusal — and a second copy of that sequence is a second
// place for the guard or the redaction to be forgotten. So the sequence lives here once and both call it.
//
// WHAT IS DELIBERATELY NOT HERE: the model check, the handshake, the authentication and the session
// open/resume. Those differ between the populations (an Ask turn sets its model per turn and resumes a
// persisted conversation), and folding them together would make one population's failure message the
// other's.

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/beardedparrott/orchicon/internal/antigravity/acp"
	"github.com/beardedparrott/orchicon/internal/guard"
)

// spawnClient resolves the tenant settings, the binary and the argv, guards the argv, and starts the ACP
// client. It returns the client, the spec that was used (for logs — its String redacts the key), and a
// cleanup the caller MUST run once the client is closed (it removes the guard shim directory).
//
// EVERY ERROR IS THE OPERATOR-FACING MESSAGE, not a wrapped internal one: the caller records it as the
// failure reason, and an auth failure surfaced as `spawn: exit status 1` is the failure mode
// authMessage exists to prevent.
func (b *Bridge) spawnClient(ctx context.Context, tenantID, execID, cwd, projectDir, workflowRunID string, inContainer bool, h acp.Handler) (*acp.Client, SpawnSpec, func(), error) {
	settings, apiKey, err := b.resolveSettings(ctx, tenantID)
	if err != nil {
		return nil, SpawnSpec{}, nil, fmt.Errorf("antigravity: resolve tenant settings: %w", err)
	}
	bin, binErr := b.resolveBinary()
	if binErr != nil {
		return nil, SpawnSpec{}, nil, binErr
	}
	baseEnv, envCleanup := b.childEnvFor(projectDir, inContainer, execID)

	spec, err := BuildSpawnSpec(bin, settings, apiKey, baseEnv)
	if err != nil {
		envCleanup()
		return nil, SpawnSpec{}, nil, errors.New(authMessage(err))
	}
	// THE ARGV GUARD RUNS BEFORE THE SPAWN, NOT AFTER. A bypass flag would defeat the whole policy
	// silently, so a spec carrying one fails the session instead of launching an ungated agent.
	if err := assertNoBypass(spec.Argv); err != nil {
		envCleanup()
		return nil, SpawnSpec{}, nil, err
	}
	b.log.Info("antigravity: launching agy", "execution", execID, "spec", loggableSpec(spec),
		"cwd", cwd, "container", inContainer)

	procSpec := spec.ProcSpec()
	procSpec.ExecID = execID
	procSpec.Cwd = cwd
	procSpec.ProjectDir = projectDir
	procSpec.WorkflowRunID = workflowRunID

	client, err := acp.Start(ctx, acp.Config{
		Factory:       b.spawnFactory(),
		Launch:        acp.LaunchOptions{Binary: spec.Argv[0]},
		Spec:          procSpec,
		Handler:       h,
		AuthMethod:    spec.AuthMethod,
		ClientName:    "orchicon",
		ClientVersion: clientVersion,
		Logger:        b.log,
	})
	if err != nil {
		envCleanup()
		return nil, spec, nil, fmt.Errorf("antigravity spawn failed: %s", authMessage(err))
	}
	return client, spec, envCleanup, nil
}

// childEnvFor builds the environment one agy child inherits, for a transport and a project root.
//
// IT TAKES THE TWO FACTS IT NEEDS rather than a manifest, because the Ask population has no manifest and
// the guard is the part that must not be skipped: an Ask conversation runs agy as a HOST process with the
// OS-level shim on PATH exactly as a worker does.
func (b *Bridge) childEnvFor(projectDir string, inContainer bool, execID string) ([]string, func()) {
	env := os.Environ()
	cleanup := func() {}
	if inContainer {
		return env, cleanup
	}
	g, err := guard.NewExecutionGuard(projectDir)
	if err != nil {
		b.log.Warn("antigravity: the OS-level execution guard could not be built for the local transport — "+
			"the child runs with the ACP policy only", "execution", execID, "error", err)
		return env, cleanup
	}
	return g.Apply(env), g.Close
}

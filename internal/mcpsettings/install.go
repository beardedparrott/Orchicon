package mcpsettings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/audit"
	"github.com/beardedparrott/orchicon/internal/db"
)

// Runtime names.
const (
	RuntimeNpx    = "npx"
	RuntimeUvx    = "uvx"
	RuntimeDocker = "docker"
)

// installTimeout bounds one auto-install run (context timeout).
const installTimeout = 60 * time.Second

// maxOutputBytes bounds captured stdout/stderr per install.
const maxOutputBytes = 64 << 10

// dryRunEnv is the CI belt-and-braces gate: when set to "1", EVERY install
// call is forced to dry-run (no exec, no DB write). Default tests set it.
const dryRunEnv = "ORCHICON_MCP_INSTALL_DRYRUN"

// DetectRuntimes reports which install runtimes are present on the host
// (exec.LookPath). Always returns all three keys.
func DetectRuntimes() map[string]bool {
	return map[string]bool{
		RuntimeNpx:    lookPathOK(RuntimeNpx),
		RuntimeUvx:    lookPathOK(RuntimeUvx),
		RuntimeDocker: lookPathOK(RuntimeDocker),
	}
}

func lookPathOK(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// installPlan is the resolved install argv + runtime for an entry.
// Command built only from catalog-derived fields or the entry's own
// command/args — no shell interpolation (exec.Command with a fixed argv,
// never sh -c).
type installPlan struct {
	runtime string // npx | uvx | docker | "" (remote_url)
	argv    []string
	display string
}

// planInstall resolves the install plan for an entry. remote_url entries
// need no install (plan with empty argv; caller marks installed).
func planInstall(e Entry) (installPlan, error) {
	mech := ""
	switch {
	case e.CatalogSlug != "":
		c, ok := catalogBySlug(e.CatalogSlug)
		if !ok {
			return installPlan{}, invalidf("unknown catalog slug %q", e.CatalogSlug)
		}
		mech = c.InstallMechanism
	case e.Command != "":
		// Manual stdio entry: install the entry's own command + args. The
		// runtime is inferred from argv[0] (npx/uvx/docker prefix) or the
		// bare executable is run directly.
		argv := append([]string{e.Command}, e.Args...)
		first := strings.ToLower(e.Command)
		switch {
		case first == RuntimeNpx:
			mech = MechanismNpx
		case first == RuntimeUvx:
			mech = MechanismUvx
		case first == RuntimeDocker:
			mech = MechanismDocker
		default:
			// Bare executable (e.g. a local binary): treat the command as a
			// direct invocation; runtime detection checks the binary itself.
			return installPlan{runtime: first, argv: argv, display: strings.Join(argv, " ")}, nil
		}
		return planForMechanism(mech, e.CatalogSlug, argv[1:]), nil
	default:
		return installPlan{}, invalidf("entry %q has no installable command (set a catalog slug or a stdio command)", e.Name)
	}
	return planForMechanism(mech, e.CatalogSlug, nil), nil
}

func planForMechanism(mech, slug string, manualArgs []string) installPlan {
	switch mech {
	case MechanismNpx:
		argv := []string{RuntimeNpx, "-y"}
		argv = append(argv, manualArgs...)
		if len(manualArgs) == 0 {
			if c, ok := catalogBySlug(slug); ok {
				argv = append(argv, c.DefaultArgs...)
			} else {
				argv = append(argv, "--version") // safe smoke check
			}
		}
		// Append the version smoke check so the install verifies the package
		// resolves and exits immediately (no server handshake).
		argv = append(argv, "--version")
		return installPlan{runtime: RuntimeNpx, argv: argv, display: strings.Join(argv, " ")}
	case MechanismUvx:
		argv := []string{RuntimeUvx}
		argv = append(argv, manualArgs...)
		if len(manualArgs) == 0 {
			if c, ok := catalogBySlug(slug); ok {
				argv = append(argv, c.DefaultArgs...)
			} else {
				argv = append(argv, "--version")
			}
		}
		argv = append(argv, "--version")
		return installPlan{runtime: RuntimeUvx, argv: argv, display: strings.Join(argv, " ")}
	case MechanismDocker:
		img := ""
		if c, ok := catalogBySlug(slug); ok && len(c.DefaultArgs) > 0 {
			img = c.DefaultArgs[0]
		}
		if img == "" && len(manualArgs) > 0 {
			img = manualArgs[0]
		}
		argv := []string{RuntimeDocker, "pull", img}
		return installPlan{runtime: RuntimeDocker, argv: argv, display: strings.Join(argv, " ")}
	case MechanismRemoteURL:
		return installPlan{runtime: "", display: "remote_url: no install needed"}
	default:
		return installPlan{runtime: "", display: fmt.Sprintf("unknown mechanism %q", mech)}
	}
}

// installResultJSON encodes an InstallResult as the install_result jsonb.
func installResultJSON(r InstallResult) []byte {
	b, _ := json.Marshal(r)
	return b
}

// InstallInput is the auto-install request.
type InstallInput struct {
	ID     string
	DryRun bool
}

// InstallOutcome is the auto-install response.
type InstallOutcome struct {
	Entry     Entry
	WouldRun  bool
	Runtime   string
	Command   string
	Available bool
}

// Install runs the explicit-only auto-install for an entry. dry_run=true
// (or the ORCHICON_MCP_INSTALL_DRYRUN=1 env gate) resolves the plan and
// reports what WOULD run without executing anything or writing the DB.
// Real installs run the plan with a 60s timeout, capture bounded output,
// and record the result on the entry.
func (s *Service) Install(ctx context.Context, tenantID string, in InstallInput) (InstallOutcome, error) {
	in.ID = strings.TrimSpace(in.ID)
	if in.ID == "" {
		return InstallOutcome{}, invalidf("id must not be empty")
	}
	tx, err := s.begin(ctx, tenantID)
	if err != nil {
		return InstallOutcome{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	row, err := db.GetMCPServer(ctx, tx.Tx, tenantID, in.ID)
	if err != nil {
		return InstallOutcome{}, err
	}
	e := entryFromRow(row)
	plan, err := planInstall(e)
	if err != nil {
		return InstallOutcome{}, err
	}

	forced := os.Getenv(dryRunEnv) == "1"
	if in.DryRun || forced {
		available := true
		if plan.runtime != "" {
			available = lookPathOK(plan.runtime)
		}
		return InstallOutcome{
			Entry:     e,
			WouldRun:  plan.runtime != "" || plan.display != "remote_url: no install needed",
			Runtime:   plan.runtime,
			Command:   plan.display,
			Available: available,
		}, nil
	}

	// remote_url: no install needed — mark installed immediately.
	if plan.runtime == "" {
		res := InstallResult{Runtime: "", Command: "remote_url: no install needed", OK: true, InstalledAt: time.Now().UTC().Format(time.RFC3339)}
		if err := db.UpdateMCPServerInstallResult(ctx, tx.Tx, tenantID, in.ID, InstallInstalled, installResultJSON(res)); err != nil {
			return InstallOutcome{}, err
		}
		if err := audit.Record(ctx, tx.Tx, audit.Entry{TenantID: tenantID, Action: "mcp_server.installed", TargetType: "mcp_server", TargetID: in.ID,
			After: audit.Snapshot(map[string]any{"runtime": "", "ok": true})}); err != nil {
			return InstallOutcome{}, fmt.Errorf("mcpsettings: audit install: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return InstallOutcome{}, err
		}
		out := InstallOutcome{Entry: e, Runtime: "", Command: "remote_url: no install needed", Available: true}
		out.Entry.InstallStatus = InstallInstalled
		out.Entry.InstallResult = res
		return out, nil
	}

	if !lookPathOK(plan.runtime) {
		res := InstallResult{Runtime: plan.runtime, Command: plan.display, OK: false,
			Error: fmt.Sprintf("runtime %q is not installed on the host — install it first (npx: npm install -g npx; uvx: pip install uv; docker: docker desktop/engine)", plan.runtime)}
		if err := db.UpdateMCPServerInstallResult(ctx, tx.Tx, tenantID, in.ID, InstallFailed, installResultJSON(res)); err != nil {
			return InstallOutcome{}, err
		}
		if err := audit.Record(ctx, tx.Tx, audit.Entry{TenantID: tenantID, Action: "mcp_server.installed", TargetType: "mcp_server", TargetID: in.ID,
			After: audit.Snapshot(map[string]any{"runtime": plan.runtime, "ok": false, "error": res.Error})}); err != nil {
			return InstallOutcome{}, fmt.Errorf("mcpsettings: audit install: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return InstallOutcome{}, err
		}
		out := InstallOutcome{Entry: e, Runtime: plan.runtime, Command: plan.display, Available: false}
		out.Entry.InstallStatus = InstallFailed
		out.Entry.InstallResult = res
		return out, nil
	}

	if err := db.UpdateMCPServerInstallResult(ctx, tx.Tx, tenantID, in.ID, InstallInstalling, []byte("{}")); err != nil {
		return InstallOutcome{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return InstallOutcome{}, err
	}

	// Run the install OUTSIDE the tx (network I/O must never hold a DB
	// transaction open).
	res := s.runInstall(ctx, plan)

	tx2, err := s.begin(ctx, tenantID)
	if err != nil {
		return InstallOutcome{}, err
	}
	defer func() { _ = tx2.Rollback(ctx) }()
	status := InstallInstalled
	if !res.OK {
		status = InstallFailed
	}
	if err := db.UpdateMCPServerInstallResult(ctx, tx2.Tx, tenantID, in.ID, status, installResultJSON(res)); err != nil {
		return InstallOutcome{}, err
	}
	if err := audit.Record(ctx, tx2.Tx, audit.Entry{TenantID: tenantID, Action: "mcp_server.installed", TargetType: "mcp_server", TargetID: in.ID,
		After: audit.Snapshot(map[string]any{"runtime": plan.runtime, "ok": res.OK, "error": res.Error})}); err != nil {
		return InstallOutcome{}, fmt.Errorf("mcpsettings: audit install: %w", err)
	}
	if err := tx2.Commit(ctx); err != nil {
		return InstallOutcome{}, err
	}
	out := InstallOutcome{Entry: e, Runtime: plan.runtime, Command: plan.display, Available: true}
	out.Entry.InstallStatus = status
	out.Entry.InstallResult = res
	return out, nil
}

// runInstall executes the resolved argv with a bounded timeout and
// captures bounded stdout/stderr. Never touches the DB.
func (s *Service) runInstall(ctx context.Context, plan installPlan) InstallResult {
	res := InstallResult{Runtime: plan.runtime, Command: plan.display}
	ctx, cancel := context.WithTimeout(ctx, installTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, plan.argv[0], plan.argv[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		res.OK = false
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			res.Error = "install timed out (60s)"
		} else if msg != "" {
			res.Error = truncateUTF8(msg, maxOutputBytes)
		} else {
			res.Error = fmt.Sprintf("install failed: %v", err)
		}
		return res
	}
	res.OK = true
	res.InstalledAt = time.Now().UTC().Format(time.RFC3339)
	return res
}

func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

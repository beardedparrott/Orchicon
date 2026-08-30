package runtimeimage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/runtime"
	"github.com/beardedparrott/orchicon/internal/version"
)

// cannedTenant is where canned (stock) runtime image rows are seeded. Both
// the dev and prod instances run the same boot seeder against their own DB,
// so each gets its own independent set of canned rows — instances are never
// shared. Mirrors the canned-worker seeder (SeedDevWorkers).
const cannedTenant = "tnt_dev"

// cannedSeedMarker prefixes the versioned seed marker comment the seeder
// appends to a canned row's dockerfile_override. A stock row carrying the
// CURRENT marker whose dockerfile_override is byte-identical to the shipped
// template is "pristine + current" — the seeder preserves it untouched. A
// stock row whose override differs (user edited it but kept the marker) is
// user-diverged — the seeder never touches it. A stock row missing the
// marker is a stale seed (or an edit that dropped the marker) — it is rolled
// forward to the current template, which bumps `version` so the row is
// rebuilt.
const cannedSeedMarker = "orchicon.seed"

// stockVariants are the shipped runtime-image templates, in build order
// (base first; :gui/:dev derive FROM it). The daemon reports them via the
// ORCHICON_RUNTIME_IMAGE / ORCHICON_RUNTIME_IMAGES allowlist. Indexes are
// referenced by variant resolution (base = the daemon's default image).
type stockVariant struct {
	File    string // embedded template path (assets.RuntimeImageTemplatesFS)
	Variant string // "base" | "gui" | "dev"
	Name    string
	Desc    string
}

var stockVariants = []stockVariant{
	{File: "deploy/runtime/Dockerfile", Variant: "base", Name: "Runtime base image",
		Desc: "The lean runtime base: system toolchain, user-space package managers (pip/npm/mise/uv/bun), no-root chowned-rootfs model."},
	{File: "deploy/runtime/Dockerfile.gui", Variant: "gui", Name: "Runtime GUI image (:gui)",
		Desc: "Base image plus headless GUI libraries (Qt offscreen via PySide6/PyQt, tkinter, X11) for GUI toolchain work."},
	{File: "deploy/runtime/Dockerfile.dev", Variant: "dev", Name: "Orchicon development image (:orchicon-dev)",
		Desc: "Go, Node, buf, atlas, baked PostgreSQL 15 + nats-server: the supervisor boots a full disposable Orchicon control plane in the container at start (Postgres -> NATS -> orchicon serve), so workers get the orchicon_* MCP tools, localhost:5432 DB tests, and http://localhost:8080 — all isolated from the host plane."},
}

// SeedCannedImages ensures one canned "stock" runtime image row exists per
// shipped runtime-image variant the daemon reports (base first). Idempotent,
// called at boot; mirrors the canned-worker seeder contract:
//
//   - Missing rows are created (source='stock', template + seed marker as the
//     dockerfile_override). If the local docker image is already current
//     (container.sh / the installer built it before the plane booted), the
//     row is marked ready with built_version = version; otherwise it is
//     auto-built asynchronously.
//   - Pristine rows are rolled forward when the shipped template changes
//     (the marker SHA changes): the override is replaced, `version` bumps,
//     built_version stays stale, and the row is rebuilt — which prunes the
//     previous version of the tag (the daemon's post-build dangling prune).
//     container.sh already rebuilt the pristine image pre-boot → the seeder
//     just records built_version = version instead of re-building.
//   - User-diverged rows (override differs from the shipped template) are
//     never touched, and user builds (spec-version label) make container.sh
//     skip the tag — so a tenant-owned build survives `make container-rebuild`.
//   - Deleted canned rows are re-seeded on the next boot, like canned workers.
//
// A missing daemon (headless serve) skips seeding entirely.
func SeedCannedImages(ctx context.Context, pool *db.Pool, log *slog.Logger, rt *runtime.Client) error {
	if rt == nil {
		log.Info("runtime daemon not configured — skipping canned runtime image seed")
		return nil
	}
	imgs, err := rt.Images(ctx)
	if err != nil || imgs == nil {
		log.Warn("canned runtime image seed: daemon images unavailable", "error", err)
		return nil // daemon down — retry next boot
	}
	infos := make(map[string]runtime.ImageInfo, len(imgs.Infos))
	for _, i := range imgs.Infos {
		infos[i.Ref] = i
	}
	svc := &Service{pool: pool, log: log, rt: rt}
	for _, ref := range imgs.Images {
		variant, ok := resolveVariant(ref, imgs.Default)
		if !ok {
			continue // operator-added daemon-only image; no shipped template to seed
		}
		if err := seedOneCannedImage(ctx, svc, ref, variant, infos[ref], imgs.Default); err != nil {
			log.Warn("canned runtime image seed failed", "tag", ref, "error", err)
		}
	}
	return nil
}

// resolveVariant maps a daemon stock tag to its shipped variant. The base is
// the daemon's default image; :gui / :orchicon-dev are matched by tag suffix
// (same rule as GetStockImageTemplate). Unknown tags (operator-added
// ORCHICON_RUNTIME_IMAGES with no shipped template) return ok=false.
func resolveVariant(ref, defaultRef string) (stockVariant, bool) {
	if ref == defaultRef {
		return stockVariants[0], true
	}
	lower := strings.ToLower(ref)
	colon := strings.LastIndex(lower, ":")
	suffix := lower
	if colon >= 0 {
		suffix = lower[colon+1:]
	}
	switch {
	case strings.HasSuffix(suffix, "gui") || strings.HasPrefix(suffix, "gui-") || strings.HasPrefix(suffix, "gui_"):
		return stockVariants[1], true
	case strings.HasSuffix(suffix, "orchicon-dev") || strings.HasSuffix(suffix, "dev") || strings.HasPrefix(suffix, "dev-") || strings.HasPrefix(suffix, "dev_"):
		return stockVariants[2], true
	}
	return stockVariant{}, false
}

// seedOneCannedImage creates or reconciles one canned stock row for a tag.
// See SeedCannedImages for the contract. `info` is the daemon's version-label
// view of the local image (may be all-empty when the image is missing).
//
// The seed-pristine test is self-consistent: the seeder writes the marker as
// sha12 of the exact template body it embeds, so a row whose override body
// hashes to its own marker is "a seed, some generation" — current markers are
// reconciled in place, stale markers are rolled forward (version bumps →
// rebuild). Any override whose body does not hash to its own marker is a user
// edit (or a marker the user removed) — the seeder never touches it, so user
// modifications survive seed bumps exactly like edited canned workers.
func seedOneCannedImage(ctx context.Context, svc *Service, ref string, variant stockVariant, info runtime.ImageInfo, defaultRef string) error {
	content, err := assets.RuntimeImageTemplatesFS.ReadFile(variant.File)
	if err != nil {
		return fmt.Errorf("read stock template %s: %w", variant.File, err)
	}
	// The shipped Dockerfiles end with a newline, so the marker forms its own
	// final line: `...# \n# orchicon.seed=<sha12-of-content>\n`. seedState
	// extracts the body as everything before the marker, which is then exactly
	// the embedded template content — so hashing it reproduces the marker.
	seedDockerfile := string(content) + fmt.Sprintf("# %s=%s\n", cannedSeedMarker, seedVersion(content))

	ttx, err := svc.pool.BeginTenantTx(ctx, cannedTenant)
	if err != nil {
		return err
	}
	defer ttx.Rollback(ctx)

	row, err := db.GetRuntimeImageByTag(ctx, ttx.Tx, cannedTenant, ref)
	if errors.Is(err, db.ErrNotFound) {
		row, err = db.CreateRuntimeImage(ctx, ttx.Tx, db.RuntimeImageRow{
			ID:                 db.NewID(),
			TenantID:           cannedTenant,
			Name:               variant.Name,
			Slug:               fmt.Sprintf("stock-%s", variant.Variant),
			Description:        variant.Desc,
			BaseImageRef:       defaultRef,
			AptPackages:        []byte("[]"),
			Toolchains:         []byte("[]"),
			Env:                []byte("{}"),
			DockerfileOverride: seedDockerfile,
			Tag:                ref,
			Status:             "draft",
			Source:             "stock",
			Version:            1,
		})
		if err != nil {
			return fmt.Errorf("create canned image: %w", err)
		}
	} else if err != nil {
		return err
	}

	if row.Source != "stock" {
		// A tenant-created row owns this tag — the seeder must not touch it.
		return nil
	}

	pristine, current := seedState(row.DockerfileOverride, content)
	if !pristine {
		// User-diverged (edited the body or removed the marker) — never touch.
		return nil
	}

	if !current {
		// Pristine row from an older seed generation: roll the seed forward —
		// replace the override with the current template + marker. This is a
		// spec edit, so `version` bumps and built_version lags — the row needs
		// a rebuild.
		status := "draft"
		row, err = db.UpdateRuntimeImage(ctx, ttx.Tx, cannedTenant, row.ID, row.Version,
			db.UpdateRuntimeImageFields{DockerfileOverride: &seedDockerfile, Status: &status})
		if err != nil {
			return fmt.Errorf("roll forward canned image %s: %w", ref, err)
		}
	}

	needsBuild := row.Status != "ready" || row.BuiltVersion != row.Version
	if needsBuild && row.Status == "failed" {
		// A previous auto-build failed — never auto-retry every boot; the
		// user rebuilds via Deploy.
		return nil
	}
	if !needsBuild {
		return nil
	}
	if err := ttx.Commit(ctx); err != nil {
		return err
	}

	// The local docker image may already be current (container.sh built it
	// before the plane booted, or a previous auto-build finished): record
	// ready + built_version = version instead of rebuilding.
	if stockImageCurrent(info, variant) {
		markReady(ctx, svc.pool, row)
		return nil
	}
	// Auto-build asynchronously so plane boot stays fast. The build core
	// re-checks status under the row lock, so concurrent boots/Deploys are
	// serialized; on success built_version = version and the daemon's
	// post-build prune drops the previous version of the tag.
	go func() {
		ctx := context.Background()
		if _, _, err := svc.buildCore(ctx, cannedTenant, row.ID, nil); err != nil {
			svc.log.Warn("canned runtime image auto-build failed", "tag", ref, "error", err)
		} else {
			svc.log.Info("canned runtime image auto-built", "tag", ref)
		}
	}()
	return nil
}

// seedState classifies a canned row's dockerfile_override against the shipped
// template content. It returns (pristine, current):
//
//	pristine=false  → the body was edited away from any seed (or the marker was
//	                  removed): user-diverged, the seeder backs off.
//	pristine=true,  current=false → an intact seed from an older generation:
//	                  roll forward + rebuild.
//	pristine=true,  current=true  → the current seed: reconcile in place.
func seedState(override string, currentContent []byte) (pristine, current bool) {
	idx := strings.LastIndex(override, "# "+cannedSeedMarker+"=")
	if idx < 0 {
		return false, false
	}
	body := override[:idx]
	rest := strings.TrimPrefix(override[idx:], "# "+cannedSeedMarker+"=")
	end := strings.IndexAny(rest, "\r\n")
	if end < 0 {
		end = len(rest)
	}
	marker := strings.TrimSpace(rest[:end])
	// The marker must be the LAST content of the override. Anything after it
	// (an edit appended below the marker line, e.g. the UI textarea cursor
	// landing at the end) means the user changed the file — back off.
	if strings.TrimSpace(rest[end:]) != "" {
		return false, false
	}
	if len(marker) != 12 || sha12String(body) != marker {
		return false, false
	}
	return true, body == string(currentContent)
}

// markReady records a canned row as built for its current spec version
// (StatusOnly — no version bump). The local image is already current.
func markReady(ctx context.Context, pool *db.Pool, row db.RuntimeImageRow) {
	status := "ready"
	built := row.Version
	ttx, err := pool.BeginTenantTx(ctx, cannedTenant)
	if err != nil {
		return
	}
	defer ttx.Rollback(ctx)
	if _, err := db.UpdateRuntimeImage(ctx, ttx.Tx, cannedTenant, row.ID, row.Version,
		db.UpdateRuntimeImageFields{Status: &status, BuiltVersion: &built, StatusOnly: true}); err != nil {
		return
	}
	_ = ttx.Commit(ctx)
}

// seedVersion is the versioned marker value for a shipped template: the first
// 12 hex chars of its SHA-256. Any template change alters the marker, so
// pristine rows roll forward (and rebuild) exactly when the shipped image
// definition changes — never on every boot.
func seedVersion(content []byte) string {
	return sha12String(string(content))
}

func sha12String(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}

func sha12Of(file string) string {
	content, err := assets.RuntimeImageTemplatesFS.ReadFile(file)
	if err != nil {
		return ""
	}
	return seedVersion(content)
}

// stockImageCurrent reports whether the local docker image `info` describes
// already matches the runtime version container.sh / the release workflow
// would build for this variant. Accepts both label formats: the release
// workflow's (<version>, <version>-gui, <version>-dev) and container.sh's
// content-hashed (<version>-<baseSha12>[-<variantSha12>]).
func stockImageCurrent(info runtime.ImageInfo, variant stockVariant) bool {
	if info.RuntimeVersion == "" {
		return false
	}
	app := version.Current().Tag
	if app == "" {
		app = "dev"
	}
	baseSha := sha12Of(stockVariants[0].File)
	mySha := sha12Of(variant.File)
	switch variant.Variant {
	case "base":
		return info.RuntimeVersion == app || info.RuntimeVersion == app+"-"+baseSha
	case "gui", "dev":
		return info.RuntimeVersion == app+"-"+variant.Variant ||
			info.RuntimeVersion == app+"-"+baseSha+"-"+mySha
	}
	return false
}

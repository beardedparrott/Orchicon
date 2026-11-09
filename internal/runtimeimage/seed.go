package runtimeimage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"log/slog"
	"strings"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/runtime"
	"github.com/beardedparrott/orchicon/internal/version"
)

// cannedTenant is where canned (stock) runtime image rows are seeded.
const cannedTenant = "tnt_dev"

// cannedSeedMarker prefixes the versioned seed marker comment the seeder appends to a canned row's
// dockerfile_override.
const cannedSeedMarker = "orchicon.seed"

// stockSlug is a variant's stable row identity.
func stockSlug(v stockVariant) string { return "stock-" + v.Variant }

// This should happen upon launching the runtime images page in the web, desktop or TUI pages." IT
// ONLY CREATES WHAT IS ABSENT AND NEVER TOUCHES A ROW THAT EXISTS.
func EnsureCatalogRows(ctx context.Context, pool *db.Pool, log *slog.Logger) {
	if pool == nil {
		return
	}
	base := firstPublishedTag()
	for _, tag := range catalogStockTags() {
		variant, ok := resolveVariant(tag, base)
		if !ok {
			continue
		}
		content, err := assets.RuntimeImageTemplatesFS.ReadFile(variant.File)
		if err != nil {
			if log != nil {
				log.Warn("catalogue reconcile: read template failed", "variant", variant.Variant, "error", err)
			}
			continue
		}
		ttx, err := pool.BeginTenantTx(ctx, cannedTenant)
		if err != nil {
			return
		}
		_, removed, err := stockRowForVariant(ctx, ttx.Tx, variant, tag, log)
		if err == nil {
			// THE ROW EXISTS.
			if removed > 0 {
				if cerr := ttx.Commit(ctx); cerr != nil {
					if log != nil {
						log.Warn("catalogue reconcile: commit duplicate collapse failed",
							"variant", variant.Variant, "error", cerr)
					}
				} else if log != nil {
					log.Info("catalogue reconcile: collapsed duplicate rows for a variant",
						"variant", variant.Variant, "removed", removed)
				}
				continue
			}
			_ = ttx.Rollback(ctx)
			continue
		}
		if !errors.Is(err, db.ErrNotFound) {
			_ = ttx.Rollback(ctx)
			continue
		}
		if _, err := db.CreateRuntimeImage(ctx, ttx.Tx, cannedRowRow(variant, tag, content, base)); err != nil {
			_ = ttx.Rollback(ctx)
			if log != nil {
				log.Warn("catalogue reconcile: create row failed", "tag", tag, "error", err)
			}
			continue
		}
		if err := ttx.Commit(ctx); err != nil {
			if log != nil {
				log.Warn("catalogue reconcile: commit failed", "tag", tag, "error", err)
			}
			continue
		}
		if log != nil {
			log.Info("catalogue row restored as not-downloaded", "tag", tag)
		}
	}
}

// cannedRowRow is the row a shipped variant is created with: source='stock', the pristine template
// plus its seed marker, and the NOT-DOWNLOADED status.
func cannedRowRow(v stockVariant, ref string, content []byte, baseRef string) db.RuntimeImageRow {
	return db.RuntimeImageRow{
		ID:                 db.NewID(),
		TenantID:           cannedTenant,
		Name:               v.Name,
		Slug:               stockSlug(v),
		Description:        v.Desc,
		BaseImageRef:       baseRef,
		AptPackages:        []byte("[]"),
		Toolchains:         []byte("[]"),
		Env:                []byte("{}"),
		DockerfileOverride: string(content) + fmt.Sprintf("# %s=%s\n", cannedSeedMarker, seedVersion(content)),
		Tag:                ref,
		Status:             RuntimeImageStatusAbsent,
		Source:             "stock",
		Version:            1,
	}
}

// stockVariants are the shipped runtime-image templates, in build order (base first; :gui/:dev
// derive FROM it).
type stockVariant struct {
	File    string // embedded template path (assets.RuntimeImageTemplatesFS)
	Variant string // "base" | "gui" | "dev" | "web-research"
	Name    string
	Desc    string
}

// PublishedTag returns the registry tag this variant is actually published at, and false when it is
// NOT published. BASE ONLY, AND THE OTHERS ARE DELIBERATELY NOT PUBLISHED.
func (v stockVariant) PublishedTag(repo string) (string, bool) {
	if v.Variant == stockVariants[0].Variant {
		return repo + ":latest", true
	}
	return "", false
}

// LocalTag is the tag an unpublished variant is fetched at: BUILT on this host from the shipped
// template, never pulled.
func (v stockVariant) LocalTag(repo string) string {
	repo = strings.TrimSpace(repo)
	if i := strings.LastIndexByte(repo, '/'); i >= 0 {
		repo = repo[i+1:]
	}
	if repo == "" {
		repo = "orchicon-runtime"
	}
	if v.Variant == stockVariants[0].Variant {
		return repo + ":latest"
	}
	return repo + ":" + v.Variant
}

// CatalogTag is where this variant lives FOR THIS HOST: its published ref when it has one, else its
// local build tag.
func (v stockVariant) CatalogTag(repo string) string {
	if tag, ok := v.PublishedTag(repo); ok {
		return tag
	}
	return v.LocalTag(repo)
}

// TagSuffix is the "<variant>-" segment a published tag carries before the version, matching the
// daemon's convention ("ghcr.io/.../orchicon-runtime:gui-latest").
func (v stockVariant) TagSuffix() string {
	if v.Variant == stockVariants[0].Variant {
		return ""
	}
	// THE DAEMON'S OWN SPELLING, verified against a live /v1/images rather than inferred from the
	// variant NAME.
	return v.Variant + "-"
}

var stockVariants = []stockVariant{
	{File: "deploy/runtime/Dockerfile", Variant: "base", Name: "Runtime base image",
		Desc: "The lean runtime base: system toolchain, user-space package managers (pip/npm/mise/uv/bun), no-root chowned-rootfs model."},
	{File: "deploy/runtime/Dockerfile.gui", Variant: "gui", Name: "Runtime GUI image (:gui)",
		Desc: "Base image plus headless GUI libraries (Qt offscreen via PySide6/PyQt, tkinter, X11) for GUI toolchain work."},
	{File: "deploy/runtime/Dockerfile.dev", Variant: "dev", Name: "Orchicon development image (:orchicon-dev)",
		Desc: "Go, Node, buf, atlas, baked PostgreSQL 15 + nats-server: the supervisor boots a full disposable Orchicon control plane in the container at start (Postgres -> NATS -> orchicon serve), so workers get the orchicon_* MCP tools, localhost:5432 DB tests, and http://localhost:8080 — all isolated from the host plane."},
	{File: "deploy/runtime/Dockerfile.web-research", Variant: "web-research", Name: "Runtime web-research image (:web-research)",
		Desc: "Base image plus a headless browser research toolchain (Playwright + Chromium, extraction helpers) for work that has to read the web."},
}

// SeedCannedImages ensures one canned "stock" runtime image row exists per shipped runtime-image
// variant the daemon reports (base first).
func SeedCannedImages(ctx context.Context, pool *db.Pool, log *slog.Logger, rt *runtime.Client) error {
	// NO DAEMON IS NOT "NO CATALOG" — that conflation is the whole Windows report.
	if rt == nil {
		log.Info("runtime daemon not configured — seeding the published runtime-image catalog so the images stay fetchable")
		return seedPublishedCatalog(ctx, pool, log, nil)
	}
	imgs, err := rt.Images(ctx)
	if err != nil || imgs == nil {
		// THE CATALOG MUST NOT DISAPPEAR WHEN THE DAEMON DOES.
		log.Warn("canned runtime image seed: daemon images unavailable — seeding the published catalog so the images stay fetchable", "error", err)
		return seedPublishedCatalog(ctx, pool, log, rt)
	}
	infos := make(map[string]runtime.ImageInfo, len(imgs.Infos))
	for _, i := range imgs.Infos {
		infos[i.Ref] = i
	}
	svc := &Service{pool: pool, log: log, rt: rt}
	covered := map[string]bool{} // stock slugs the daemon already reported
	for _, ref := range imgs.Images {
		variant, ok := resolveVariant(ref, imgs.Default)
		if !ok {
			continue // operator-added daemon-only image; no shipped template to seed
		}
		covered[stockSlug(variant)] = true
		if err := seedOneCannedImage(ctx, svc, ref, variant, infos[ref], imgs.Default); err != nil {
			log.Warn("canned runtime image seed failed", "tag", ref, "error", err)
		}
	}
	// THEN THE PUBLISHED CATALOGUE FILLS THE GAPS.
	base := firstPublishedTag()
	for _, tag := range catalogStockTags() {
		variant, ok := resolveVariant(tag, base)
		if !ok || covered[stockSlug(variant)] {
			continue
		}
		// An all-empty ImageInfo means "no local evidence" — the row keeps built_version stale, so the
		// presence check reports it as not downloaded and the fetch button appears.
		if err := seedOneCannedImage(ctx, svc, tag, variant, runtime.ImageInfo{}, base); err != nil {
			log.Warn("canned runtime image seed failed", "tag", tag, "error", err)
		}
	}
	return nil
}

// seedPublishedCatalog seeds one row per shipped variant from the PUBLISHED tags, with no daemon
// involved.
func seedPublishedCatalog(ctx context.Context, pool *db.Pool, log *slog.Logger, rt *runtime.Client) error {
	svc := &Service{pool: pool, log: log, rt: rt}
	base := firstPublishedTag()
	for _, tag := range catalogStockTags() {
		variant, ok := resolveVariant(tag, base)
		if !ok {
			continue
		}
		// An all-empty ImageInfo means "no local evidence" — the row keeps built_version stale, so
		// the presence check reports it as not downloaded and the fetch button appears.
		if err := seedOneCannedImage(ctx, svc, tag, variant, runtime.ImageInfo{}, base); err != nil {
			log.Warn("canned runtime image seed failed", "tag", tag, "error", err)
		}
	}
	return nil
}

// PublishedStockBase is the registry reference the released runtime images live at.
const PublishedStockBase = "ghcr.io/beardedparrott/orchicon-runtime:latest"

// splitImageRef splits "repo:tag" into its parts. A ref with no tag yields "latest", matching the
// engine's own default — the daemon does the same, so the two agree on what a tagless ref means.
func splitImageRef(ref string) (repo, tag string) {
	if i := strings.LastIndexByte(ref, ':'); i > strings.LastIndexByte(ref, '/') {
		return ref[:i], ref[i+1:]
	}
	return ref, "latest"
}

// firstPublishedTag is the repository's default tag — the base image.
func firstPublishedTag() string {
	tags := publishedStockTags()
	if len(tags) == 0 {
		return PublishedStockBase
	}
	return tags[0]
}

// catalogStockTags lists the tag every shipped variant lives at on THIS host, base first: the
// published ref for the base, and a local build tag for the variants that are no longer published.
func catalogStockTags() []string {
	repo, _ := splitImageRef(PublishedStockBase)
	out := make([]string, 0, len(stockVariants))
	for _, v := range stockVariants {
		out = append(out, v.CatalogTag(repo))
	}
	return out
}

// publishedStockTags lists the released tag for every variant that IS actually published, base first.
//
// THE TAGS ARE THE REGISTRY'S, NOT A GUESS. The first version of this derived them from the variant
// NAME — ":gui-latest", ":dev-latest", ":web-research-latest" — and ALL THREE were wrong. Checked
// against the registry:
//
//	ghcr.io/beardedparrott/orchicon-runtime:latest        EXISTS
//	ghcr.io/beardedparrott/orchicon-runtime:gui-latest    MISSING
//	ghcr.io/beardedparrott/orchicon-runtime:dev-latest    MISSING
//
// so a fetch of a seeded row failed with exactly what the operator saw:
//
//	reading manifest dev-latest in ghcr.io/beardedparrott/orchicon-runtime: manifest unknown
//
// release.yml pushes `:latest`, `:gui` and `:dev` (plus per-version tags). Those three are the
// catalog.
//
// WEB-RESEARCH IS ABSENT ON PURPOSE: it has a shipped Dockerfile but release.yml does not build or
// push it, so there is nothing to fetch. Seeding it produced a row whose only possible action was a
// guaranteed failure. A variant with no published tag is simply not in the downloadable catalog; it
// can still be built locally from its template if someone asks for it by tag.
func publishedStockTags() []string {
	repo, _ := splitImageRef(PublishedStockBase)
	out := make([]string, 0, len(stockVariants))
	for _, v := range stockVariants {
		t, ok := v.PublishedTag(repo)
		if !ok {
			continue
		}
		out = append(out, t)
	}
	return out
}

// NOTE: there is deliberately no second variant resolver here.

// resolveVariant maps a daemon stock tag to its shipped variant.
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
	case strings.HasSuffix(suffix, "web-research") || strings.HasPrefix(suffix, "web-research-") || strings.HasPrefix(suffix, "web-research_"):
		return stockVariants[3], true
	case strings.HasSuffix(suffix, "gui") || strings.HasPrefix(suffix, "gui-") || strings.HasPrefix(suffix, "gui_"):
		return stockVariants[1], true
	case strings.HasSuffix(suffix, "orchicon-dev") || strings.HasSuffix(suffix, "dev") || strings.HasPrefix(suffix, "dev-") || strings.HasPrefix(suffix, "dev_"):
		return stockVariants[2], true
	}
	return stockVariant{}, false
}

// stockRowForVariant finds THE stock row for a variant, collapsing duplicates left by the old
// tag-keyed seeding.
func stockRowForVariant(ctx context.Context, tx pgx.Tx, variant stockVariant, ref string, log *slog.Logger) (db.RuntimeImageRow, int, error) {
	slug := stockSlug(variant)
	rows, err := db.ListRuntimeImages(ctx, tx, db.RuntimeImageFilter{TenantID: cannedTenant, Source: "stock"})
	if err != nil {
		return db.RuntimeImageRow{}, 0, err
	}
	var mine []db.RuntimeImageRow
	for _, r := range rows {
		if r.Slug == slug {
			mine = append(mine, r)
		}
	}
	if len(mine) == 0 {
		return db.RuntimeImageRow{}, 0, db.ErrNotFound
	}
	removed := 0
	// Prefer, in order: the row already on the preferred tag (nothing has to move, so the rename cannot
	// collide with a sibling that still holds that tag), then the one furthest through its build.
	preferred := ref
	keep := mine[0]
	score := func(r db.RuntimeImageRow) int {
		n := 0
		if r.Tag == preferred {
			n += 1000
		}
		return n + r.BuiltVersion
	}
	for _, r := range mine[1:] {
		if score(r) > score(keep) {
			keep = r
		}
	}
	for _, r := range mine {
		if r.ID == keep.ID {
			continue
		}
		content, cerr := assets.RuntimeImageTemplatesFS.ReadFile(variant.File)
		if cerr == nil {
			if pristine, _ := seedState(r.DockerfileOverride, content); !pristine {
				log.Info("canned runtime image: keeping a diverged duplicate — it carries operator edits",
					"slug", slug, "tag", r.Tag)
				continue
			}
		}
		if derr := db.DeleteRuntimeImage(ctx, tx, cannedTenant, r.ID); derr != nil {
			log.Warn("canned runtime image: could not remove a duplicate row", "slug", slug, "tag", r.Tag, "error", derr)
			continue
		}
		removed++
		log.Info("canned runtime image: removed a duplicate row for this variant", "slug", slug, "tag", r.Tag)
	}
	return keep, removed, nil
}

// seedOneCannedImage creates or reconciles one canned stock row for a tag.
func seedOneCannedImage(ctx context.Context, svc *Service, ref string, variant stockVariant, info runtime.ImageInfo, defaultRef string) error {
	content, err := assets.RuntimeImageTemplatesFS.ReadFile(variant.File)
	if err != nil {
		return fmt.Errorf("read stock template %s: %w", variant.File, err)
	}
	// The shipped Dockerfiles end with a newline, so the marker forms its own final line: `...# \n#
	// orchicon.seed=<sha12-of-content>\n`.
	seedDockerfile := string(content) + fmt.Sprintf("# %s=%s\n", cannedSeedMarker, seedVersion(content))

	ttx, err := svc.pool.BeginTenantTx(ctx, cannedTenant)
	if err != nil {
		return err
	}
	defer ttx.Rollback(ctx)

	// LOOK UP BY VARIANT (slug), NOT BY TAG — and that is the fix for the duplicated rows.
	row, duplicatesRemoved, err := stockRowForVariant(ctx, ttx.Tx, variant, ref, svc.log)
	if errors.Is(err, db.ErrNotFound) {
		row, err = db.CreateRuntimeImage(ctx, ttx.Tx, db.RuntimeImageRow{
			ID:                 db.NewID(),
			TenantID:           cannedTenant,
			Name:               variant.Name,
			Slug:               stockSlug(variant),
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

	// reconciled records that this pass changed the row's TAG (duplicate deletes are separate): both
	// live in this transaction, so a path that returns without committing discards them.
	reconciled := false

	// RECONCILE THE TAG, so one row tracks whichever tag this variant is known by NOW.
	if row.Tag != ref {
		newTag := ref
		moved, terr := db.UpdateRuntimeImage(ctx, ttx.Tx, cannedTenant, row.ID, row.Version,
			db.UpdateRuntimeImageFields{Tag: &newTag, StatusOnly: true})
		if terr != nil {
			return fmt.Errorf("reconcile canned image tag %s -> %s: %w", row.Tag, ref, terr)
		}
		svc.log.Info("canned runtime image: tag reconciled to this host's", "slug", row.Slug, "from", row.Tag, "to", ref)
		row = moved
		reconciled = true
	}

	rolledForward := false
	if !current {
		// Pristine row from an older seed generation: roll the seed forward — replace the override with the
		// current template + marker.
		status := "draft"
		row, err = db.UpdateRuntimeImage(ctx, ttx.Tx, cannedTenant, row.ID, row.Version,
			db.UpdateRuntimeImageFields{DockerfileOverride: &seedDockerfile, Status: &status})
		if err != nil {
			return fmt.Errorf("roll forward canned image %s: %w", ref, err)
		}
		rolledForward = true
	}

	switch decideSeedAction(row, info, variant, pristine, current) {
	case seedSkip:
		// Nothing to reconcile.
		if rolledForward || reconciled || duplicatesRemoved > 0 {
			if err := ttx.Commit(ctx); err != nil {
				return err
			}
		}
		return nil
	case seedMarkReady:
		if err := ttx.Commit(ctx); err != nil {
			return err
		}
		markReady(ctx, svc.pool, row)
		return nil
	case seedMarkAbsent:
		if err := ttx.Commit(ctx); err != nil {
			return err
		}
		markAbsent(ctx, svc.pool, svc.log, row)
		return nil
	}

	if err := ttx.Commit(ctx); err != nil {
		return err
	}
	// Auto-build asynchronously so plane boot stays fast.
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

// seedAction is the canned-row reconciliation decision: what the seeder must
// do with one stock row this boot.
type seedAction int

const (
	// seedSkip leaves the row alone.
	seedSkip seedAction = iota
	// seedMarkReady records status=ready + built_version=version (the local
	// image is already current).
	seedMarkReady
	// seedMarkAbsent records status=absent: the row is CORRECT, but its image
	// is not downloaded locally and is NOT being built.
	seedMarkAbsent
	// seedBuild auto-builds the image asynchronously.
	seedBuild
)

// RuntimeImageStatusAbsent is the runtime_images.status of a row whose spec is correct but whose
// image is not present locally and is not being built.
const RuntimeImageStatusAbsent = "absent"

// decideSeedAction is the WHOLE canned-row reconciliation policy, as a pure function: no DB, no
// engine, no goroutines.
func decideSeedAction(row db.RuntimeImageRow, info runtime.ImageInfo, variant stockVariant, pristine, current bool) seedAction {
	if row.Source != "stock" {
		// A tenant-created row owns this tag — never touch it.
		return seedSkip
	}
	if !pristine {
		// User-diverged (edited the body or removed the marker) — never touch.
		return seedSkip
	}
	if current && row.Status == "ready" && row.BuiltVersion == row.Version {
		// Current seed, already built: reconcile in place.
		return seedSkip
	}
	if row.Status == "failed" {
		// A previous auto-build failed — never auto-retry every boot; the user rebuilds via Deploy.
		return seedSkip
	}
	if info.Present && stockImageCurrent(info, variant) {
		// The local image is already current (container.sh built it before the
		// plane booted, or a previous auto-build finished).
		return seedMarkReady
	}
	if !info.Present {
		// THE TRAP, CLOSED. Nothing is downloaded for this tag, so there is nothing to reconcile: mark the
		// row absent and DO NOT BUILD.
		return seedMarkAbsent
	}
	// Present but not current: a stale seed (roll-forward) or a locally-built image carrying no version
	// label.
	return seedBuild
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
	// The marker must be the LAST content of the override.
	if strings.TrimSpace(rest[end:]) != "" {
		return false, false
	}
	if len(marker) != 12 || sha12String(body) != marker {
		return false, false
	}
	return true, body == string(currentContent)
}

// markAbsent records a canned row as ABSENT: its spec is correct, but the image is not present
// locally and no build was started for it.
func markAbsent(ctx context.Context, pool *db.Pool, log *slog.Logger, row db.RuntimeImageRow) {
	status := RuntimeImageStatusAbsent
	ttx, err := pool.BeginTenantTx(ctx, cannedTenant)
	if err != nil {
		return
	}
	defer ttx.Rollback(ctx)
	if _, err := db.UpdateRuntimeImage(ctx, ttx.Tx, cannedTenant, row.ID, row.Version,
		db.UpdateRuntimeImageFields{Status: &status, StatusOnly: true}); err != nil {
		return
	}
	if err := ttx.Commit(ctx); err != nil {
		return
	}
	if log != nil {
		log.Info("canned runtime image not downloaded — awaiting an explicit fetch", "tag", row.Tag)
	}
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

// seedVersion is the versioned marker value for a shipped template: the first 12 hex chars of its
// SHA-256.
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

// stockImageCurrent reports whether the local image `info` describes already matches the runtime
// version container.sh / the release workflow would build for this variant.
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
	case "gui", "dev", "web-research":
		return info.RuntimeVersion == app+"-"+variant.Variant ||
			info.RuntimeVersion == app+"-"+baseSha+"-"+mySha
	}
	return false
}

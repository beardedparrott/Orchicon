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
	Variant string // "base" | "gui" | "dev" | "web-research"
	Name    string
	Desc    string
}

// PublishedTag returns the registry tag this variant is actually published at, and false when it is
// NOT published (web-research today): release.yml pushes ":latest" for the base and ":<variant>" for
// the others, plus per-version tags this catalog does not need.
func (v stockVariant) PublishedTag(repo string) (string, bool) {
	switch v.Variant {
	case stockVariants[0].Variant:
		return repo + ":latest", true
	case "gui", "dev":
		return repo + ":" + v.Variant, true
	}
	return "", false
}

// TagSuffix is the "<variant>-" segment a published tag carries before the version, matching the
// daemon's convention ("ghcr.io/.../orchicon-runtime:gui-latest"). The base variant has NO suffix —
// it IS the repository's default tag — which is why this returns "" for it rather than "base-".
func (v stockVariant) TagSuffix() string {
	if v.Variant == stockVariants[0].Variant {
		return ""
	}
	// THE DAEMON'S OWN SPELLING, verified against a live /v1/images rather than inferred from the
	// variant NAME: cmd/orchicon's runtimeImagesAllowlist emits "<repo>:gui-<tag>" and
	// "<repo>:dev-<tag>" — so the development image is ":dev-latest", NOT ":orchicon-dev-latest".
	// The first version of this returned the display name's spelling, which would have seeded a
	// SECOND row for the same artifact and split the catalog in two.
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
	// NO DAEMON IS NOT "NO CATALOG" — that conflation is the whole Windows report.
	//
	// This used to return here, and it is the branch a Windows host takes: no container engine means
	// no runtime daemon ("runtime daemon not configured"), so the seeder skipped entirely and the
	// Runtime Images page — which lists ROWS — showed NOTHING. "In Windows, it doesn't have ANY of the
	// runtime images." With no rows there is no fetch button either, so it was a dead end rather than
	// a missing feature, and no amount of fixing the fetch would have made it reachable.
	//
	// The shipped variants and their templates are compile-time facts. Only their LOCAL PRESENCE needs
	// a daemon. So the catalog is seeded from the published tags as rows with no built_version, which
	// is precisely the "available to download, not downloaded" state the page must be able to show —
	// and which the presence check already understands.
	if rt == nil {
		log.Info("runtime daemon not configured — seeding the published runtime-image catalog so the images stay fetchable")
		return seedPublishedCatalog(ctx, pool, log, nil)
	}
	imgs, err := rt.Images(ctx)
	if err != nil || imgs == nil {
		// THE CATALOG MUST NOT DISAPPEAR WHEN THE DAEMON DOES.
		//
		// REPORTED: "In Windows, it doesn't have ANY of the runtime images." That is this branch: a
		// host whose container engine is not set up yet has no daemon, so the daemon cannot report
		// which stock images it knows — and the Runtime Images page, which lists ROWS, showed nothing
		// at all. The operator then has no fetch button to press because there is nothing to press it
		// on, which is a dead end rather than a missing feature.
		//
		// The shipped variants and their templates are COMPILE-TIME facts (stockVariants +
		// assets.RuntimeImageTemplatesFS); only their LOCAL PRESENCE depends on the daemon. So they are
		// seeded regardless, as rows with no built_version — "available to download, not downloaded" —
		// which is precisely the state the page has to be able to show.
		//
		// THE TAG FALLBACK IS THE PUBLISHED ONE. Without a daemon there is no locally-built tag to ask
		// about, so the row gets the released reference the image would be pulled from
		// (publishedStockTags). On a host that later gets an engine, the next boot re-seeds from the
		// daemon — which reconciles by TAG, so a locally-built tag wins and the row moves to it.
		log.Warn("canned runtime image seed: daemon images unavailable — seeding the published catalog so the images stay fetchable", "error", err)
		return seedPublishedCatalog(ctx, pool, log, rt)
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

// seedPublishedCatalog seeds one row per shipped variant from the PUBLISHED tags, with no daemon
// involved. It is the path a host takes before it has a container engine, and the fallback when the
// daemon is unreachable — one implementation for both, so the two cannot seed different sets.
func seedPublishedCatalog(ctx context.Context, pool *db.Pool, log *slog.Logger, rt *runtime.Client) error {
	svc := &Service{pool: pool, log: log, rt: rt}
	base := firstPublishedTag()
	for _, tag := range publishedStockTags() {
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

// PublishedStockBase is the registry reference the released runtime images live at. It mirrors the
// daemon's own default (cmd/orchicon/runtime.go: ORCHICON_RUNTIME_IMAGE), because a row seeded from it
// and a daemon image must name the SAME artifact — two spellings would seed the catalog twice.
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

// NOTE: there is deliberately no second variant resolver here. resolveVariant already maps a tag to
// its shipped variant and handles every spelling in use (:gui-, :dev-, :orchicon-dev-, :web-research-),
// including the ones an operator sets through ORCHICON_RUNTIME_IMAGES. A second mapping is how the
// catalog path and the daemon path would come to disagree about which variant a tag names — and that
// disagreement seeds the same artifact twice.

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
// tag-keyed seeding. Returns db.ErrNotFound when there is none yet.
//
// COLLAPSING IS DELIBERATE AND IS NOT DATA LOSS. These rows are machine-managed (source='stock') and
// the catalog is theirs to manage; the image itself lives in the container engine, not in the row, so
// removing a duplicate record does not untag or delete anything. Only PRISTINE seeds are removed —
// a row whose Dockerfile an operator edited is theirs and is never touched, matching the seeder's
// standing rule.
//
// When duplicates exist the survivor is: the one already carrying the preferred tag if there is one
// (so nothing has to move), else the most-built one, so the row that records real work is the one
// kept.
func stockRowForVariant(ctx context.Context, tx pgx.Tx, variant stockVariant, ref string, log *slog.Logger) (db.RuntimeImageRow, int, error) {
	slug := fmt.Sprintf("stock-%s", variant.Variant)
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
	// Prefer, in order: the row already on the preferred tag (nothing has to move, so the rename
	// cannot collide with a sibling that still holds that tag), then the one furthest through its
	// build, so the row recording real work survives.
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

	// LOOK UP BY VARIANT (slug), NOT BY TAG — and that is the fix for the duplicated rows.
	//
	// REPORTED with a screenshot: six rows for three images, each variant listed TWICE — one
	// "not downloaded" (the published tag this catalog seeded) and one "ready" (the local tag the
	// daemon reported). Keying on the tag creates a NEW row for every tag the same variant is known
	// by, so a host that first seeded from the published catalog and later got a container engine
	// ends up with both.
	//
	// The identity that matters is the VARIANT: stock-base is stock-base whether it is called
	// orchicon-runtime:local or ghcr.io/...:latest. So the row is found by slug, its tag is
	// RECONCILED to the preferred one, and any duplicates left by the old behaviour are collapsed.
	row, duplicatesRemoved, err := stockRowForVariant(ctx, ttx.Tx, variant, ref, svc.log)
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

	// reconciled records that this pass changed the row's TAG (duplicate deletes are separate): both
	// live in this transaction, so a path that returns without committing discards them.
	reconciled := false

	// RECONCILE THE TAG, so one row tracks whichever tag this variant is known by NOW.
	//
	// This is the other half of the duplication fix: a row found by slug may still carry the tag it
	// was created under — the published one, from a boot before this host had a container engine. Left
	// alone it would be a row whose tag no local image answers to, so a fetch would try to pull a
	// registry ref for an image that is already built here. Moving the tag onto the preferred one also
	// keeps built_version meaningful, because built_version describes the image behind THAT tag.
	//
	// A tag move does NOT bump `version`: the spec did not change, only which local name it refers to.
	// StatusOnly is exactly that statement.
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
		rolledForward = true
	}

	switch decideSeedAction(row, info, variant, pristine, current) {
	case seedSkip:
		// Nothing to reconcile. A roll-forward still has to be persisted, or
		// the next boot would roll it forward again (and bump `version` a
		// second time). The same is true of a TAG RECONCILIATION and of the
		// duplicate deletes above: they live in this transaction, so returning
		// without committing discards them — which is exactly what happened
		// when this was first written (the DB was unchanged after a reseed that
		// had logged the rename).
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

// RuntimeImageStatusAbsent is the runtime_images.status of a row whose spec is
// correct but whose image is not present locally and is not being built.
// Distinct from `draft` (never built) and `failed` (a build was attempted and
// failed), and stored as free text — the status column has no CHECK
// constraint, so this needs no migration.
const RuntimeImageStatusAbsent = "absent"

// decideSeedAction is the WHOLE canned-row reconciliation policy, as a pure
// function: no DB, no engine, no goroutines. That is the seam AC 10/11 are
// tested through.
//
// `info` is the daemon's report for the row's tag and `pristine`/`current`
// come from seedState. `row` is the row as it stands AFTER any roll-forward
// the caller applied.
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
		// A previous auto-build failed — never auto-retry every boot; the user
		// rebuilds via Deploy. Kept ABOVE the absent case on purpose: a failed
		// build must not be silently reclassified as "not downloaded".
		return seedSkip
	}
	if info.Present && stockImageCurrent(info, variant) {
		// The local image is already current (container.sh built it before the
		// plane booted, or a previous auto-build finished).
		return seedMarkReady
	}
	if !info.Present {
		// THE TRAP, CLOSED. Nothing is downloaded for this tag, so there is
		// nothing to reconcile: mark the row absent and DO NOT BUILD. Building
		// here would compile Go, Node, PostgreSQL and Playwright on a first boot
		// that deliberately pulled nothing — far slower and heavier than the
		// pull this whole change exists to avoid, and it would happen silently
		// in the background. The image is fetched at opt-in time instead.
		return seedMarkAbsent
	}
	// Present but not current: a stale seed (roll-forward) or a locally-built
	// image carrying no version label. Both keep the pre-change behaviour —
	// auto-build — so only true ABSENCE changes.
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

// markAbsent records a canned row as ABSENT: its spec is correct, but the
// image is not present locally and no build was started for it. It mirrors
// markReady exactly (same tenant tx, StatusOnly — no version bump) and
// deliberately leaves built_version alone: absence is not a build outcome, and
// keeping the last built version is what lets a later fetch/Deploy prove it
// produced the version the row asked for.
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

// stockImageCurrent reports whether the local image `info` describes already
// matches the runtime version container.sh / the release workflow would build
// for this variant. Accepts both label formats: the release workflow's
// (<version>, <version>-gui, <version>-dev) and container.sh's content-hashed
// (<version>-<baseSha12>[-<variantSha12>]).
//
// An image with NO version label reports false — but that is NOT a verdict
// that it must be built: "present without a version label" is a locally-built
// image, not a missing one. Whether the image exists at all is
// ImageInfo.Present, which the seeder's decideSeedAction consults first.
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

// Package runtimeimage implements the RuntimeImageService Connect handler.
//
// A RuntimeImage is a tenant buildable container image derived from the
// Orchicon runtime base. This service persists the build spec, delegates
// the actual `docker build` to the runtime daemon (the only process with
// the Docker socket), streams build logs, and exposes the merged stock +
// custom image list that feeds the work-item runtime_image dropdown.
package runtimeimage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/beardedparrott/orchicon/internal/containerengine"
	"log/slog"
	"os/exec"
	"regexp"
	goruntime "runtime"
	"strings"
	"time"

	"connectrpc.com/connect"
	assets "github.com/beardedparrott/orchicon"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	apiv1connect "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/audit"
	"github.com/beardedparrott/orchicon/internal/auth"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/runtime"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	maxNameLen        = 200
	maxSlugLen        = 64
	maxDescriptionLen = 4000
	maxDockerfileLen  = 1 << 16
	maxTagLen         = 256
	maxBuildLogLen    = 1 << 20
)

// slugPattern matches the image slug (same rule as project slugs:
// lowercase words separated by single hyphens).
var regexpSlug = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// Service implements the RuntimeImageService Connect handler.
type Service struct {
	pool *db.Pool
	log  *slog.Logger
	rt   *runtime.Client // nil in headless (no daemon)
	base string          // resolved base image ref from the daemon (cached at first Images call)
	apiv1connect.UnimplementedRuntimeImageServiceHandler
}

var _ apiv1connect.RuntimeImageServiceHandler = (*Service)(nil)

// New constructs a RuntimeImageService handler. rt may be nil (headless
// `orchicon serve` without a runtime daemon) — build/delete then fail
// with a clear error, while CRUD + list still work.
func New(pool *db.Pool, log *slog.Logger, rt *runtime.Client) *Service {
	return &Service{pool: pool, log: log, rt: rt}
}

// requireTenant resolves the caller's tenant id from context.
func requireTenant(ctx context.Context) (string, error) {
	tid := tenant.FromContext(ctx)
	if tid == "" {
		return "", connect.NewError(connect.CodeUnauthenticated, errors.New("tenant required"))
	}
	return tid, nil
}

// ensureBase resolves the daemon's base image ref (cached on first call).
// Returns "" when no daemon is configured.
func (s *Service) ensureBase(ctx context.Context) string {
	if s.base != "" {
		return s.base
	}
	if s.rt == nil {
		return ""
	}
	imgs, err := s.rt.Images(ctx)
	if err != nil || imgs == nil {
		return ""
	}
	s.base = imgs.Default
	return s.base
}

func (s *Service) CreateRuntimeImage(ctx context.Context, req *connect.Request[apiv1.CreateRuntimeImageRequest]) (*connect.Response[apiv1.CreateRuntimeImageResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	name, err := validateName(m.Name)
	if err != nil {
		return nil, err
	}
	slug, err := validateSlug(m.Slug)
	if err != nil {
		return nil, err
	}
	desc, _ := bound(m.Description, maxDescriptionLen)
	apt, err := validateJSONArray(m.AptPackages, "apt_packages")
	if err != nil {
		return nil, err
	}
	toolchains, err := validateJSONArray(m.Toolchains, "toolchains")
	if err != nil {
		return nil, err
	}
	envJSON, err := validateJSONObject(m.Env, "env")
	if err != nil {
		return nil, err
	}
	override, err := bound(m.DockerfileOverride, maxDockerfileLen)
	if err != nil {
		return nil, err
	}
	tag := strings.TrimSpace(m.Tag)
	if tag == "" {
		tag = slug + ":latest"
	}
	if _, err := bound(tag, maxTagLen); err != nil {
		return nil, err
	}

	base := s.ensureBase(ctx)

	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)

	row, err := db.CreateRuntimeImage(ctx, ttx.Tx, db.RuntimeImageRow{
		ID:                 db.NewID(),
		TenantID:           tenantID,
		Name:               name,
		Slug:               slug,
		Description:        desc,
		BaseImageRef:       base,
		AptPackages:        apt,
		Toolchains:         toolchains,
		Env:                envJSON,
		DockerfileOverride: override,
		Tag:                tag,
		Status:             "draft",
		Source:             "custom",
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create runtime image: %w", err))
	}
	if err := recordAudit(ctx, ttx.Tx, tenantID, "runtime_image.created", "runtime_image", row.ID,
		nil, audit.Snapshot(runtimeImageAuditSnapshot(row))); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit runtime_image.created: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&apiv1.CreateRuntimeImageResponse{RuntimeImage: toProto(row)}), nil
}

func (s *Service) GetRuntimeImage(ctx context.Context, req *connect.Request[apiv1.GetRuntimeImageRequest]) (*connect.Response[apiv1.GetRuntimeImageResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, err
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	row, err := db.GetRuntimeImage(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("runtime image not found"))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&apiv1.GetRuntimeImageResponse{RuntimeImage: toProto(row)}), nil
}

func (s *Service) ListRuntimeImages(ctx context.Context, req *connect.Request[apiv1.ListRuntimeImagesRequest]) (*connect.Response[apiv1.ListRuntimeImagesResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, err
	}
	status := ""
	if req.Msg.Status != nil {
		status = req.Msg.Status.String()
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	rows, err := db.ListRuntimeImages(ctx, ttx.Tx, db.RuntimeImageFilter{TenantID: tenantID, Status: status, Search: req.Msg.Search})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*apiv1.RuntimeImage, 0, len(rows))
	for _, r := range rows {
		out = append(out, toProto(r))
	}
	return connect.NewResponse(&apiv1.ListRuntimeImagesResponse{RuntimeImages: out}), nil
}

func (s *Service) UpdateRuntimeImage(ctx context.Context, req *connect.Request[apiv1.UpdateRuntimeImageRequest]) (*connect.Response[apiv1.UpdateRuntimeImageResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	f := db.UpdateRuntimeImageFields{}
	if m.Name != nil {
		v, err := validateName(*m.Name)
		if err != nil {
			return nil, err
		}
		f.Name = &v
	}
	if m.Slug != nil {
		v, err := validateSlug(*m.Slug)
		if err != nil {
			return nil, err
		}
		f.Slug = &v
	}
	if m.Description != nil {
		v, _ := bound(*m.Description, maxDescriptionLen)
		f.Description = &v
	}
	if m.AptPackages != nil {
		v, err := validateJSONArray(*m.AptPackages, "apt_packages")
		if err != nil {
			return nil, err
		}
		f.AptPackages = &v
	}
	if m.Toolchains != nil {
		v, err := validateJSONArray(*m.Toolchains, "toolchains")
		if err != nil {
			return nil, err
		}
		f.Toolchains = &v
	}
	if m.Env != nil {
		v, err := validateJSONObject(*m.Env, "env")
		if err != nil {
			return nil, err
		}
		f.Env = &v
	}
	if m.DockerfileOverride != nil {
		v, err := bound(*m.DockerfileOverride, maxDockerfileLen)
		if err != nil {
			return nil, err
		}
		f.DockerfileOverride = &v
	}
	if m.Tag != nil {
		v, err := bound(*m.Tag, maxTagLen)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(v) != "" {
			f.Tag = &v
		}
	}

	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	// Revert a READY image to DRAFT on edit so it must be rebuilt before
	// it can be used again.
	cur, err := db.GetRuntimeImage(ctx, ttx.Tx, tenantID, m.Id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("runtime image not found"))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	status := "draft"
	if cur.Status == "building" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("cannot edit an image while it is building"))
	}
	if cur.Status != "draft" {
		f.Status = &status
	}
	row, err := db.UpdateRuntimeImage(ctx, ttx.Tx, tenantID, m.Id, int(m.Version), f)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("optimistic concurrency conflict — reload and retry"))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := recordAudit(ctx, ttx.Tx, tenantID, "runtime_image.updated", "runtime_image", row.ID,
		audit.Snapshot(runtimeImageAuditSnapshot(cur)), audit.Snapshot(runtimeImageAuditSnapshot(row))); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit runtime_image.updated: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&apiv1.UpdateRuntimeImageResponse{RuntimeImage: toProto(row)}), nil
}

func (s *Service) DeleteRuntimeImage(ctx context.Context, req *connect.Request[apiv1.DeleteRuntimeImageRequest]) (*connect.Response[apiv1.DeleteRuntimeImageResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, err
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	row, err := db.GetRuntimeImage(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("runtime image not found"))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// Gate on no active workflow run referencing the tag.
	active, err := activeRunUsesImage(ctx, ttx.Tx, tenantID, row.Tag)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if active {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			errors.New("cannot delete image: an active workflow run uses it"))
	}
	if err := db.DeleteRuntimeImage(ctx, ttx.Tx, tenantID, req.Msg.Id); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("runtime image not found"))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := recordAudit(ctx, ttx.Tx, tenantID, "runtime_image.deleted", "runtime_image", row.ID,
		audit.Snapshot(runtimeImageAuditSnapshot(row)), nil); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit runtime_image.deleted: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// Remove the local docker image (best-effort — the daemon may be
	// absent or the image may already be gone). Canned (stock) rows and
	// the daemon's configured base image are exempt: deleting a canned row
	// just removes the spec (the seeder re-creates it on the next boot and
	// keeps the pristine container.sh image), and the base image is the
	// substrate every other image derives from — it must never be removed.
	if s.rt != nil && row.Tag != "" && row.Source != "stock" && row.Tag != s.ensureBase(ctx) {
		if err := s.rt.RemoveImage(context.Background(), row.Tag); err != nil {
			s.log.Warn("runtime image docker rmi failed", "tag", row.Tag, "error", err)
		}
	}
	return connect.NewResponse(&apiv1.DeleteRuntimeImageResponse{}), nil
}

// errAlreadyUpToDate is the sentinel buildCore returns when the image spec is
// already built (ready + built_version == version): no docker build ran.
var errAlreadyUpToDate = errors.New("runtime image already up to date")

// FetchRuntimeImage DOWNLOADS the row's published image (`podman pull` /
// `docker pull`) and streams the pull output — the on-demand FETCH.
//
// It is deliberately NOT BuildRuntimeImage: Deploy BUILDS a spec from a
// Dockerfile, Fetch DOWNLOADS an image someone else already built. Runtime
// images are never pulled at install time (a project defaults to
// execution_mode=local and needs no image), so this is how an image becomes
// locally present when an operator opts into runtime containers.
//
// Idempotent, and it never downloads inside a workflow run: an image already
// present locally short-circuits in the daemon with an immediate exit 0 and no
// engine command, and the final message carries skipped=true. On success the
// row is marked ready with built_version = version, so a subsequent Deploy
// reports "up to date" instead of rebuilding an image that was just fetched.
func (s *Service) FetchRuntimeImage(ctx context.Context, req *connect.Request[apiv1.FetchRuntimeImageRequest], stream *connect.ServerStream[apiv1.FetchRuntimeImageResponse]) error {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return err
	}
	if s.rt == nil {
		return connect.NewError(connect.CodeFailedPrecondition,
			errors.New("runtime daemon is not configured — cannot fetch a runtime image"))
	}
	// Read the row and release the tx BEFORE the pull: a multi-GB download
	// must not hold a database transaction open.
	readTx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	row, err := db.GetRuntimeImage(ctx, readTx.Tx, tenantID, req.Msg.GetId())
	_ = readTx.Rollback(ctx)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return connect.NewError(connect.CodeNotFound, errors.New("runtime image not found"))
		}
		return connect.NewError(connect.CodeInternal, err)
	}
	if strings.TrimSpace(row.Tag) == "" {
		return connect.NewError(connect.CodeFailedPrecondition,
			errors.New("runtime image has no tag to fetch"))
	}

	// FETCH IS NOT ALWAYS A PULL, and getting that wrong is the reported bug:
	//
	//	runtime image fetch failed for orchicon-runtime:local: runtime image pull: runtime daemon: request failed
	//
	// after the daemon stopped 404ing, the same fetch failed again inside podman with
	//
	//	short-name "orchicon-runtime:local" did not resolve to an alias
	//
	// because `orchicon-runtime:local` IS NOT A REGISTRY REFERENCE. It is a tag this host BUILDS
	// from a shipped Dockerfile (deploy/runtime/Dockerfile*), so `podman pull` can never succeed for
	// it — there is no registry holding it. The old comment said "Fetch DOWNLOADS a published image",
	// which is true only of the CUSTOM case.
	//
	// SO THE ROW'S SOURCE DECIDES:
	//   stock  -> BUILD from the shipped template it already carries. The template lives in
	//             dockerfile_override (seed.go writes it there), so this is a normal build of a known
	//             spec and the existing build path — including its up-to-date short-circuit and its
	//             structured failure reporting — does the work.
	//   custom -> PULL, which is what "fetch" means for an image someone else published.
	//
	// A stock row with no override cannot be built; that is a seeding defect, and it is reported as
	// one rather than being turned into a confusing pull failure.
	var (
		exit    int
		skipped bool
		perr    error
	)
	// THE DECISION IS THE TAG, NOT THE SOURCE — corrected after the first attempt got it wrong.
	//
	// Source alone is the wrong axis: a STOCK image can be either kind. A development host builds
	// `orchicon-runtime:local` from a shipped Dockerfile (Source=stock, must BUILD), while a released
	// image is published at `ghcr.io/beardedparrott/orchicon-runtime:latest` (Source may also be
	// stock, must PULL). Routing on Source would make one of them fail.
	//
	// The property that actually decides it is whether the tag names a REGISTRY. A reference with a
	// registry and repository — "ghcr.io/org/name:tag", "docker.io/library/x:1" — is something `pull`
	// can fetch. A bare "name:tag" is a SHORT NAME: on a fresh podman with no registries.conf it is
	// exactly the string that produced
	//
	//	short-name "orchicon-runtime:local" did not resolve to an alias
	//
	// and it is, by construction, an image this host builds.
	if !isRegistryRef(row.Tag) {
		// OWNED END TO END, both ways. buildCore is the authority on a build's outcome — it writes
		// the row ready, or failed WITH failure_reason/failed_step/log_tail/failure_category — so this
		// path must return rather than fall through to recordFetchOutcome, which would overwrite that
		// verdict with the far less useful "fetch failed". A stock fetch therefore reports exactly
		// what a Deploy reports, because it IS one.
		if strings.TrimSpace(row.DockerfileOverride) == "" {
			return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
				"stock runtime image %q has no shipped template to build from — the canned seeder did "+
					"not populate dockerfile_override, so there is nothing to fetch", row.Tag))
		}
		_ = stream.Send(&apiv1.FetchRuntimeImageResponse{Log: fmt.Sprintf(
			"%s is a locally-built stock image, not a registry reference — building it from the shipped template\n", row.Tag)})
		built, bexit, berr := s.buildCore(ctx, tenantID, row.ID, func(chunk string) error {
			if chunk == "" {
				return nil
			}
			return stream.Send(&apiv1.FetchRuntimeImageResponse{Log: chunk})
		})
		exit, perr = bexit, berr
		skipped = errors.Is(berr, errAlreadyUpToDate)
		if berr == nil || skipped {
			if err := stream.Send(&apiv1.FetchRuntimeImageResponse{
				Status:  imageStatusProto(built.Status),
				Tag:     row.Tag,
				Skipped: skipped,
			}); err != nil {
				return err
			}
			// THE ENGINE CHECK RUNS ON THIS PATH TOO. A build that just succeeded is the strongest
			// signal that runtime containers are about to be used — and returning above without this
			// would have checked the engine only for PULLS, i.e. never on the dev hosts where the
			// local tag is what gets fetched.
			s.reportEngineReadiness(ctx, stream)
			return nil
		}
		// A BUILD FAILURE, reported with buildCore's own verdict — not re-derived here.
		failMsg := built.FailureReason
		if failMsg == "" {
			failMsg = built.Error
		}
		if failMsg == "" {
			failMsg = berr.Error()
		}
		if err := stream.Send(&apiv1.FetchRuntimeImageResponse{
			Status: imageStatusProto(built.Status),
			Error:  failMsg,
			Tag:    row.Tag,
		}); err != nil {
			return err
		}
		return connect.NewError(connect.CodeInternal,
			fmt.Errorf("runtime image fetch failed for %s: %s", row.Tag, failMsg))
	} else {
		// PULL. The daemon owns presence + idempotency; nothing is orphaned by downloading.
		exit, skipped, perr = s.rt.PullImage(ctx, row.Tag, func(ev runtime.AgentEvent) error {
			if ev.Data == "" {
				return nil
			}
			return stream.Send(&apiv1.FetchRuntimeImageResponse{Log: ev.Data})
		})
	}
	failed := perr != nil || exit != 0

	// Persist the outcome. StatusOnly: a fetch is not a spec edit, so
	// `version` never bumps. On success the row is READY at its current
	// version (that is what built_version records). On failure the row KEEPS
	// its status — a failed download is a configuration/network error, not a
	// build outcome, so it must not be recorded as `failed` (which the seeder
	// treats as "a build was attempted").
	final, uerr := s.recordFetchOutcome(ctx, tenantID, row, perr, exit)
	if uerr != nil {
		s.log.Warn("persist runtime image fetch outcome failed", "id", row.ID, "error", uerr)
	}

	failMsg := ""
	if failed {
		failMsg = "runtime image fetch failed"
		if perr != nil {
			failMsg = perr.Error()
		}
	}
	if err := stream.Send(&apiv1.FetchRuntimeImageResponse{
		Status:  imageStatusProto(final.Status),
		Error:   failMsg,
		Tag:     row.Tag,
		Skipped: skipped,
	}); err != nil {
		return err
	}
	if failed {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("runtime image fetch failed for %s: %s", row.Tag, failMsg))
	}

	// THE ENGINE IS CHECKED *AFTER* A SUCCESSFUL FETCH, because that is the moment the operator has
	// declared they intend to use runtime containers — and the moment the engine matters.
	//
	// It is deliberately NOT a failure of the fetch: the image IS downloaded, and rolling that back
	// would be a lie. It is reported as log lines on the same stream, ending with a plain statement of
	// what is wrong and what fixes it.
	//
	// THE MACHINE STEP IS A REAL REPAIR on macOS and Windows: an absent or stopped podman VM is
	// created or started here, with no elevation and no prompt, so the common fresh-desktop case heals
	// itself. An engine that is not installed at all is REPORTED with its exact install command — a
	// stream RPC has no terminal to confirm an elevation on, and escalating silently is worse than
	// saying which command to run.
	s.reportEngineReadiness(ctx, stream)
	return nil
}

// reportEngineReadiness validates the container engine on the fetch stream, repairing what it can.
//
// Every line goes through the same log channel the fetch used, so the operator sees it in the dialog
// they are already watching rather than in a server log they are not.
func (s *Service) reportEngineReadiness(ctx context.Context, stream *connect.ServerStream[apiv1.FetchRuntimeImageResponse]) {
	say := func(format string, args ...any) {
		_ = stream.Send(&apiv1.FetchRuntimeImageResponse{Log: fmt.Sprintf(format, args...) + "\n"})
	}
	// THE PROOF COSTS ONE TRIVIAL CONTAINER (containerengine.DefaultVerifyImage, ~3 MB, already
	// local after the first time and `--rm`-ed). That is the same rung the installer insists on, and
	// it is the difference between "podman is installed" and "podman can run our work" — the state
	// this check exists to report. Paid once per fetch, on a stream the operator is already watching.
	say("\nchecking the container engine — the image is fetched, but runs need a usable engine\n")

	res := containerengine.EnsureReady(ctx, containerengine.DefaultProbe(), engineLogWriter{send: say})
	switch {
	case res.Err == nil:
		if res.Fixed {
			say("container engine ready (%s %s) — the podman machine was started for you\n",
				res.Status.Engine, res.Status.Version)
		} else {
			say("container engine ready (%s %s)\n", res.Status.Engine, res.Status.Version)
		}
	case res.InstallCommand != "":
		// NOT INSTALLED. The one case this call cannot fix, stated with the command that can.
		say("Container engine NOT available: %v\n", res.Err)
		say("  This host needs podman. Orchicon cannot install it from here — a request has no\n")
		say("  terminal to confirm an elevation on. Run, then press Fetch again:\n")
		say("    %s\n", res.InstallCommand)
		s.warnPerPlatform(say)
	default:
		say("Container engine NOT usable: %v\n", res.Err)
		s.warnPerPlatform(say)
	}
	say("the image itself is fetched and available either way\n")
}

// warnPerPlatform adds the platform-specific prerequisite that the generic message cannot know: a
// Windows host needs WSL2 behind podman, and a Mac needs the podman machine (which is a Linux VM).
//
// SAID ON EVERY NON-READY PATH RATHER THAN ONLY WHEN THE ENGINE IS ABSENT, because a podman that is
// installed but cannot start its VM is exactly the state where the platform prerequisite is the
// thing the operator is missing, and "podman is not usable" does not point at WSL or a VM.
func (s *Service) warnPerPlatform(say func(string, ...any)) {
	switch goruntime.GOOS {
	case "windows":
		// PODMAN ON WINDOWS, NOT PODMAN INSIDE WSL — and the difference decides whether anything
		// works. This probe runs `podman` from the WINDOWS PATH. A podman installed *inside* a WSL
		// distro is invisible to it, so advising "apt-get install podman in WSL" would send the
		// operator to a state this plane cannot use — which is worse than no advice.
		//
		// Podman for Windows (winget RedHat.Podman, or Podman Desktop) is what belongs on PATH. It
		// uses WSL2 as its BACKEND and manages its own machine, so WSL2 must be enabled first.
		//
		// WHICH STEP IS MISSING IS DETECTED, not guessed. On the reporting machine WSL was already
		// present and podman was not, so a fixed three-step list would have led with a step that was
		// done. The first line names the actual gap.
		_, wslErr := exec.LookPath("wsl")
		if wslErr != nil {
			say("  Windows: WSL2 is not enabled, and podman for Windows needs it as its backend.\n")
			say("    1. enable it:                     wsl --install        (then reboot)\n")
			say("    2. install podman on Windows:     winget install --id RedHat.Podman -e\n")
		} else {
			say("  Windows: WSL is present; podman for Windows is what is missing.\n")
			say("    1. install podman on Windows:     winget install --id RedHat.Podman -e\n")
		}
		say("    (podman installed INSIDE a WSL distro is NOT visible to this plane — it must be on\n")
		say("     the Windows PATH)\n")
		say("    Orchicon then creates/starts the podman machine for you on the next fetch.\n")
	case "darwin":
		say("  macOS: runtime containers run in podman's Linux VM, which must exist and be running.\n")
		say("    podman machine init --now    (Orchicon does this for you when podman is installed)\n")
	}
}

// engineLogWriter adapts the engine's io.Writer output onto the fetch stream, line by line, so the
// operator watches the repair happen instead of waiting on a silent call.
type engineLogWriter struct {
	send func(string, ...any)
}

func (w engineLogWriter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		w.send("%s", string(p))
	}
	return len(p), nil
}

// isRegistryRef reports whether a tag names a registry repository, which is what `engine pull` can
// fetch, as opposed to a bare short name that only ever exists because this host built it.
//
// THE RULE IS PODMAN'S OWN: a reference is registry-qualified when its first path component looks
// like a host — it contains a dot ("ghcr.io"), a colon (a port), or is exactly "localhost". Anything
// else ("orchicon-runtime:local", "ubuntu:24.04") is a short name that resolves through
// registries.conf or not at all, and is not a download this feature can perform.
//
// Deliberately NOT "contains a slash": "beardedparrott/orchicon-runtime:latest" has a slash and is
// still a short name (Docker Hub), which is why the dot/colon/localhost test is the one that matches
// the engine's behaviour rather than a plausible-looking approximation of it.
func isRegistryRef(tag string) bool {
	ref := strings.TrimSpace(tag)
	if ref == "" {
		return false
	}
	// Strip any digest, then the tag, leaving "host/path".
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		ref = ref[:i]
	}
	name := ref
	if i := strings.LastIndexByte(name, ':'); i > strings.LastIndexByte(name, '/') {
		name = name[:i]
	}
	slash := strings.IndexByte(name, '/')
	if slash < 0 {
		return false // no path at all: a plain short name
	}
	first := name[:slash]
	if first == "localhost" {
		return true
	}
	return strings.ContainsAny(first, ".:")
}

// recordFetchOutcome writes the row state a fetch leaves behind: ready at the
// current spec version on success (clearing stale failure fields), untouched on
// failure except for the error message. Returns the row as persisted.
func (s *Service) recordFetchOutcome(ctx context.Context, tenantID string, row db.RuntimeImageRow, pullErr error, exit int) (db.RuntimeImageRow, error) {
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return row, err
	}
	defer ttx.Rollback(ctx)
	fields := db.UpdateRuntimeImageFields{StatusOnly: true}
	if pullErr != nil || exit != 0 {
		msg := "runtime image fetch failed"
		if pullErr != nil {
			msg = pullErr.Error()
		}
		fields.Error = &msg
	} else {
		ready := "ready"
		built := row.Version
		empty := ""
		fields.Status = &ready
		fields.BuiltVersion = &built
		fields.Error = &empty
		fields.FailureReason = &empty
		fields.FailedStep = &empty
		fields.LogTail = &empty
		fields.FailureCategory = &empty
		if err := recordAudit(ctx, ttx.Tx, tenantID, "runtime_image.fetched", "runtime_image", row.ID,
			nil, audit.Snapshot(map[string]any{"tag": row.Tag, "status": "ready"})); err != nil {
			return row, err
		}
	}
	final, err := db.UpdateRuntimeImage(ctx, ttx.Tx, tenantID, row.ID, row.Version, fields)
	if err != nil {
		return row, err
	}
	if err := ttx.Commit(ctx); err != nil {
		return row, err
	}
	return final, nil
}

// BuildRuntimeImage delegates `docker build` to the runtime daemon and
// streams log chunks until the build succeeds or fails.
func (s *Service) BuildRuntimeImage(ctx context.Context, req *connect.Request[apiv1.BuildRuntimeImageRequest], stream *connect.ServerStream[apiv1.BuildRuntimeImageResponse]) error {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return err
	}
	if s.rt == nil {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("runtime daemon not configured (headless serve)"))
	}
	final, exit, err := s.buildCore(ctx, tenantID, req.Msg.Id, func(chunk string) error {
		if chunk == "" {
			return nil
		}
		return stream.Send(&apiv1.BuildRuntimeImageResponse{Log: chunk})
	})
	if err != nil && isStreamDropped(err) {
		// Stream dropped but daemon still building: surface as unavailable so UI can show building + retry.
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("%s", final.FailureReason))
	}
	if errors.Is(err, errAlreadyUpToDate) {
		s.log.Info("runtime image already up to date (spec version) — skipping rebuild",
			"id", req.Msg.Id, "tag", final.Tag, "version", final.Version)
		msg := fmt.Sprintf("runtime image already up to date (spec version %d) — skipping rebuild", final.Version)
		if err := stream.Send(&apiv1.BuildRuntimeImageResponse{Log: msg}); err != nil {
			return err
		}
		return stream.Send(&apiv1.BuildRuntimeImageResponse{
			Status:  imageStatusProto(final.Status),
			Tag:     final.Tag,
			Skipped: true,
		})
	}
	if err != nil {
		return err
	}
	if err := stream.Send(&apiv1.BuildRuntimeImageResponse{
		Status:          imageStatusProto(final.Status),
		Error:           final.Error,
		FailureReason:   final.FailureReason,
		FailedStep:      final.FailedStep,
		LogTail:         final.LogTail,
		FailureCategory: final.FailureCategory,
		Tag:             final.Tag,
	}); err != nil {
		return err
	}
	if exit != 0 {
		reason := final.FailureReason
		if reason == "" {
			reason = final.Error
		}
		return connect.NewError(connect.CodeInternal, fmt.Errorf("image build failed: %s", reason))
	}
	return nil
}

// buildCore is the shared engine behind BuildRuntimeImage (RPC, streams
// logs) and the canned-image seeder's auto-build (logs discarded). It runs
// the transaction-safe build flow: row lock, skip gate, mark building,
// daemon build, persist outcome. onLog is invoked with each build log chunk
// (may be nil). Returns the final row, the docker build exit code, and an
// error; errAlreadyUpToDate means the spec is already built and nothing ran.
func (s *Service) buildCore(ctx context.Context, tenantID, id string, onLog func(string) error) (db.RuntimeImageRow, int, error) {
	// Read the row under FOR UPDATE and keep the lock until the row is
	// marked 'building' (one transaction). This is the OCC barrier that
	// serializes concurrent Deploys: the marking transition is StatusOnly,
	// so it does NOT bump `version` — without the row lock, two
	// simultaneous Deploys could both pass the checks below and both run
	// `docker build` on the same tag. The lock is held only for the
	// read+mark window, never across the docker build itself.
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return db.RuntimeImageRow{}, 1, connect.NewError(connect.CodeInternal, err)
	}
	row, err := db.GetRuntimeImageForUpdate(ctx, ttx.Tx, tenantID, id)
	if err != nil {
		_ = ttx.Rollback(ctx)
		if errors.Is(err, db.ErrNotFound) {
			return row, 1, connect.NewError(connect.CodeNotFound, errors.New("runtime image not found"))
		}
		return row, 1, connect.NewError(connect.CodeInternal, err)
	}
	if row.Status == "building" {
		_ = ttx.Rollback(ctx)
		return row, 1, connect.NewError(connect.CodeFailedPrecondition, errors.New("image is already building"))
	}

	// Rebuild gate: when the row is ready and records the CURRENT spec
	// version as already built, the local docker image is exactly what the
	// spec describes — re-deploying is a no-op (no docker build, no prune).
	// This is the acceptance-criteria mechanism for custom images: the
	// version is the change signal, and the image is only rebuilt (and only
	// pruned) when a spec edit bumped it past built_version.
	if row.Status == "ready" && row.Tag != "" && row.BuiltVersion == row.Version {
		_ = ttx.Rollback(ctx)
		s.log.Info("runtime image already up to date (spec version) — skipping rebuild",
			"id", row.ID, "tag", row.Tag, "version", row.Version)
		return row, 0, errAlreadyUpToDate
	}

	// Mark building + clear the old log/error. StatusOnly=true: this is a
	// build-flow transition, not a spec edit, so `version` (the "spec
	// changed" signal) must NOT bump.
	building := "building"
	clearErr := ""
	row, err = db.UpdateRuntimeImage(ctx, ttx.Tx, tenantID, row.ID, row.Version, db.UpdateRuntimeImageFields{
		Status:     &building,
		Error:      &clearErr,
		StatusOnly: true,
	})
	if err != nil {
		_ = ttx.Rollback(ctx)
		return row, 1, connect.NewError(connect.CodeFailedPrecondition, errors.New("optimistic concurrency conflict — reload and retry"))
	}
	if err := recordAudit(ctx, ttx.Tx, tenantID, "runtime_image.build_started", "runtime_image", row.ID,
		nil, audit.Snapshot(map[string]any{"tag": row.Tag, "status": "building"})); err != nil {
		_ = ttx.Rollback(ctx)
		return row, 1, connect.NewError(connect.CodeInternal, fmt.Errorf("audit runtime_image.build_started: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return row, 1, connect.NewError(connect.CodeInternal, err)
	}

	// THE DAEMON'S BASE WINS, and that is a correctness requirement rather than a preference.
	//
	// runtime's validateBuild refuses any base_image_ref that is not the DAEMON'S OWN base image
	// ("base_image_ref must be the runtime base image (%s)"), and rewriteDockerfileBase then forces
	// the FROM line to it anyway. So row.BaseImageRef is advisory: it is what the row was seeded
	// against, not what will be built from.
	//
	// THAT MATTERS NOW because the catalog is seeded before a host has any engine (see
	// seedPublishedCatalog), so those rows carry the PUBLISHED base — and on a development host whose
	// daemon default is a locally-built tag, a build sent with the published base is rejected before
	// it starts. Resolving the daemon's base first makes a seeded row buildable on whichever host it
	// lands on, which is the whole point of seeding it.
	//
	// Falls back to the row's ref only when the daemon cannot say (no daemon, or it did not answer) —
	// in which case the build fails at the daemon anyway, and the row's ref is the more informative
	// thing to have tried.
	base := s.ensureBase(ctx)
	if base == "" {
		base = row.BaseImageRef
	}
	if base == "" {
		return row, 1, connect.NewError(connect.CodeFailedPrecondition, errors.New("no base image configured"))
	}
	df := generatedDockerfile(row, base)

	var logBuf strings.Builder
	exit, err := s.rt.BuildImage(ctx, runtime.BuildRequest{
		Slug:        row.Slug,
		Tag:         row.Tag,
		Base:        base,
		Dockerfile:  df,
		SpecVersion: row.Version,
	}, func(ev runtime.AgentEvent) error {
		if ev.Data != "" {
			logBuf.WriteString(ev.Data)
			logBuf.WriteString("\n")
			if onLog != nil {
				return onLog(ev.Data)
			}
		}
		return nil
	})
	if err != nil {
		exit = 1
	}

	// Persist the outcome. StatusOnly=true again — no version bump — and on
	// success stamp built_version = the spec version that was just built, so
	// the next Deploy short-circuits. On failure built_version stays stale
	// (status != ready), forcing a rebuild on retry.
	status := "ready"
	buildLog := truncate(logBuf.String(), maxBuildLogLen)
	failMsg := ""
	var bf BuildFailure
	isStreamDrop := err != nil && isStreamDropped(err)
	if exit != 0 {
		status = "failed"
		if err != nil {
			failMsg = err.Error()
		}
		if failMsg == "" {
			failMsg = "docker build failed"
		}
		// Classify the failure for actionable hints.
		bf = ClassifyBuildLog(buildLog, exit, failMsg)
		// Stream drop: don't mark failed immediately if daemon still building.
		if isStreamDrop {
			bf.Category = "stream"
			if s.rt != nil {
				if building, perr := s.rt.IsBuilding(ctx, row.Tag); perr == nil && building {
					bf.Reason = "stream disconnected — build still running"
					bf.FailedStep = ""
					// Keep row building with a visible signal; caller handles CodeUnavailable.
					status = "building"
					failMsg = bf.Reason
				} else if perr != nil {
					bf.Reason = "daemon unreachable — build may still be running on host"
					failMsg = bf.Reason
				}
			}
		}
		if bf.Reason != "" {
			failMsg = bf.Reason
		}
	}
	ttx, err = s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return row, exit, connect.NewError(connect.CodeInternal, err)
	}
	fields := db.UpdateRuntimeImageFields{
		Status:     &status,
		BuildLog:   &buildLog,
		Error:      &failMsg,
		StatusOnly: true,
	}
	if status == "failed" {
		fr := bf.Reason
		fs := bf.FailedStep
		lt := bf.LogTail
		fc := bf.Category
		if fr == "" {
			fr = failMsg
		}
		fields.FailureReason = &fr
		fields.FailedStep = &fs
		fields.LogTail = &lt
		fields.FailureCategory = &fc
	} else if exit == 0 {
		// Success clears stale failure fields.
		empty := ""
		fields.FailureReason = &empty
		fields.FailedStep = &empty
		fields.LogTail = &empty
		fields.FailureCategory = &empty
		built := row.Version
		fields.BuiltVersion = &built
	} else if status == "building" && isStreamDrop {
		// Stream dropped but still building: surface reason without marking failed.
		fr := bf.Reason
		fs := bf.FailedStep
		lt := bf.LogTail
		fc := bf.Category
		fields.FailureReason = &fr
		fields.FailedStep = &fs
		fields.LogTail = &lt
		fields.FailureCategory = &fc
	}
	final, err := db.UpdateRuntimeImage(ctx, ttx.Tx, tenantID, row.ID, row.Version, fields)
	if err != nil {
		_ = ttx.Rollback(ctx)
		s.log.Warn("persist runtime image build outcome failed", "id", row.ID, "error", err)
		return row, exit, connect.NewError(connect.CodeInternal, fmt.Errorf("persist build outcome: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return final, exit, connect.NewError(connect.CodeInternal, err)
	}
	if status == "building" && isStreamDrop {
		return final, exit, &runtime.StreamDroppedError{Err: fmt.Errorf("%s", failMsg)}
	}
	return final, exit, nil
}

// ListAvailableRuntimeImages returns the merged dropdown list for the
// work-item runtime_image field: the daemon's stock images plus the
// tenant's ready custom images, with the base image as the default.

func (s *Service) CancelRuntimeImageBuild(ctx context.Context, req *connect.Request[apiv1.CancelRuntimeImageBuildRequest]) (*connect.Response[apiv1.CancelRuntimeImageBuildResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, err
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	row, err := db.GetRuntimeImage(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("runtime image not found"))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if row.Status != "building" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("image is not building"))
	}
	if int(req.Msg.Version) != 0 && int(req.Msg.Version) != row.Version {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("optimistic concurrency conflict — reload and retry"))
	}
	if s.rt != nil && row.Tag != "" {
		_ = s.rt.CancelBuild(ctx, row.Tag)
	}
	failed := "failed"
	reason := "cancelled by user"
	cat := "stream"
	tail := truncate(row.BuildLog, 8*1024)
	emptyStep := ""
	updated, err := db.UpdateRuntimeImage(ctx, ttx.Tx, tenantID, row.ID, row.Version, db.UpdateRuntimeImageFields{
		Status:          &failed,
		Error:           &reason,
		FailureReason:   &reason,
		FailedStep:      &emptyStep,
		LogTail:         &tail,
		FailureCategory: &cat,
		StatusOnly:      true,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&apiv1.CancelRuntimeImageBuildResponse{RuntimeImage: toProto(updated)}), nil
}

func (s *Service) ResetRuntimeImage(ctx context.Context, req *connect.Request[apiv1.ResetRuntimeImageRequest]) (*connect.Response[apiv1.ResetRuntimeImageResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, err
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	row, err := db.GetRuntimeImage(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("runtime image not found"))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if row.Status != "building" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("image is not building — reset only applies to stuck building images"))
	}
	if int(req.Msg.Version) != 0 && int(req.Msg.Version) != row.Version {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("optimistic concurrency conflict — reload and retry"))
	}
	// If daemon still reports building, prefer Cancel path.
	if s.rt != nil && row.Tag != "" {
		if building, perr := s.rt.IsBuilding(ctx, row.Tag); perr == nil && building {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("build still running on daemon — use Cancel instead"))
		}
	}
	failed := "failed"
	reason := "build abandoned (no image produced) — daemon restarted or stream lost; retry Deploy"
	cat := "stream"
	tail := truncate(row.BuildLog, 8*1024)
	emptyStep := ""
	updated, err := db.UpdateRuntimeImage(ctx, ttx.Tx, tenantID, row.ID, row.Version, db.UpdateRuntimeImageFields{
		Status:          &failed,
		Error:           &reason,
		FailureReason:   &reason,
		FailedStep:      &emptyStep,
		LogTail:         &tail,
		FailureCategory: &cat,
		StatusOnly:      true,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&apiv1.ResetRuntimeImageResponse{RuntimeImage: toProto(updated)}), nil
}

func (s *Service) ListAvailableRuntimeImages(ctx context.Context, req *connect.Request[apiv1.ListAvailableRuntimeImagesRequest]) (*connect.Response[apiv1.ListAvailableRuntimeImagesResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, err
	}
	resp := &apiv1.ListAvailableRuntimeImagesResponse{}
	if s.rt != nil {
		if imgs, err := s.rt.Images(ctx); err == nil && imgs != nil {
			resp.StockImages = imgs.Images
			resp.DefaultImage = imgs.Default
			s.base = imgs.Default
		}
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	tags, err := db.ReadyRuntimeImageTags(ctx, ttx.Tx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// Canned stock rows that are ready carry the same tag as the daemon's
	// stock images — the dropdown must not offer them twice. Drop any ready
	// row tag that the daemon already reports as stock.
	stock := make(map[string]bool, len(resp.StockImages))
	for _, s := range resp.StockImages {
		stock[s] = true
	}
	for _, t := range tags {
		if !stock[t] {
			resp.CustomImages = append(resp.CustomImages, t)
		}
	}
	if resp.DefaultImage == "" && len(resp.StockImages) > 0 {
		resp.DefaultImage = resp.StockImages[0]
	}
	return connect.NewResponse(resp), nil
}

// ReconcileStuckBuilding transitions stale building rows (>ttl) to failed
// when the daemon no longer reports them as building and no image exists.
// Called periodically and at boot to heal ghost builds without daemon restart.
func (s *Service) ReconcileStuckBuilding(ctx context.Context, ttl time.Duration) (int, error) {
	if s == nil || s.pool == nil {
		return 0, nil
	}
	if ttl == 0 {
		ttl = 5 * time.Minute
	}
	// List building rows older than ttl
	ttx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer ttx.Rollback(ctx)
	interval := fmt.Sprintf("%d seconds", int(ttl.Seconds()))
	rows, err := ttx.Query(ctx, `SELECT id, tenant_id, tag, version, build_log, updated_at FROM runtime_images WHERE status='building' AND updated_at < now() - $1::interval`, interval)
	if err != nil {
		return 0, err
	}
	type stuck struct {
		id, tenant, tag, log string
		ver                  int
	}
	var list []stuck
	for rows.Next() {
		var r stuck
		var upd time.Time
		if err := rows.Scan(&r.id, &r.tenant, &r.tag, &r.ver, &r.log, &upd); err != nil {
			continue
		}
		list = append(list, r)
	}
	rows.Close()
	if len(list) == 0 {
		return 0, nil
	}
	count := 0
	for _, r := range list {
		if s.rt != nil && r.tag != "" {
			if building, perr := s.rt.IsBuilding(ctx, r.tag); perr != nil {
				// daemon unreachable — leave stuck building for next tick, don't falsely mark failed
				continue
			} else if building {
				continue
			}
			// Not building: if image exists with matching spec_version, heal to ready; else failed
			if info, perr := s.rt.InspectImage(ctx, r.tag); perr == nil && info != nil && info.Exists {
				if info.SpecVersion == fmt.Sprintf("%d", r.ver) {
					status := "ready"
					empty := ""
					built := r.ver
					if _, err := db.UpdateRuntimeImage(ctx, ttx, r.tenant, r.id, r.ver, db.UpdateRuntimeImageFields{Status: &status, Error: &empty, FailureReason: &empty, FailedStep: &empty, LogTail: &empty, FailureCategory: &empty, BuiltVersion: &built, StatusOnly: true}); err == nil {
						count++
					}
					continue
				}
			}
		}
		failed := "failed"
		reason := "build abandoned (no image produced) — daemon restarted or stream lost; retry Deploy"
		cat := "stream"
		tail := truncate(r.log, 8*1024)
		emptyStep := ""
		_, err := db.UpdateRuntimeImage(ctx, ttx, r.tenant, r.id, r.ver, db.UpdateRuntimeImageFields{Status: &failed, Error: &reason, FailureReason: &reason, FailedStep: &emptyStep, LogTail: &tail, FailureCategory: &cat, StatusOnly: true})
		if err == nil {
			count++
		}
	}
	_ = ttx.Commit(ctx)
	return count, nil
}

// StartReconciler runs a periodic background reconciler for stuck building images (60s tick).
func (s *Service) StartReconciler(ctx context.Context) {
	if s == nil || s.pool == nil {
		return
	}
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.ReconcileStuckBuilding(ctx, 0); err != nil {
				s.log.Warn("runtime image periodic reconcile failed", "error", err)
			}
		}
	}
}

// activeRunUsesImage reports whether any non-terminal workflow run has
// resolved to the given image tag.
func activeRunUsesImage(ctx context.Context, tx pgx.Tx, tenantID, tag string) (bool, error) {
	var n int
	err := tx.QueryRow(ctx,
		`SELECT count(*) FROM workflow_runs
		 WHERE tenant_id = $1 AND runtime_image = $2
		   AND status IN ('pending','running','paused')`, tenantID, tag).Scan(&n)
	return n > 0, err
}

// GetStockImageTemplate returns the shipped Dockerfile for a stock image
// (base / :gui / :orchicon-dev) so users can see how a shipped image is
// built and copy the pattern for a custom one. Lookup is by tag suffix:
// a tag ending in "-gui"/":gui" maps to the GUI variant, "-dev"/
// ":orchicon-dev" to the dev variant, anything else to the base.
func (s *Service) GetStockImageTemplate(ctx context.Context, req *connect.Request[apiv1.GetStockImageTemplateRequest]) (*connect.Response[apiv1.GetStockImageTemplateResponse], error) {
	tag := strings.TrimSpace(req.Msg.Tag)
	if tag == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("tag required"))
	}
	// Resolve which shipped Dockerfile the tag maps to.
	var file, name, desc string
	lower := strings.ToLower(tag)
	colon := strings.LastIndex(lower, ":")
	suffix := lower
	if colon >= 0 {
		suffix = lower[colon+1:]
	}
	switch {
	case strings.HasSuffix(suffix, "gui") || strings.Contains(suffix, "gui-") || strings.Contains(suffix, "gui_"):
		file = "deploy/runtime/Dockerfile.gui"
		name = "Runtime GUI image (:gui)"
		desc = "Base image plus headless GUI libraries (Qt offscreen via PySide6/PyQt, tkinter, X11) for GUI toolchain work."
	case strings.HasSuffix(suffix, "dev") || strings.HasSuffix(suffix, "orchicon-dev"):
		file = "deploy/runtime/Dockerfile.dev"
		name = "Orchicon development image (:orchicon-dev)"
		desc = "Go, Node, buf, atlas, and a baked PostgreSQL 15 for building and DB-testing the Orchicon repo in-sandbox."
	default:
		file = "deploy/runtime/Dockerfile"
		name = "Runtime base image"
		desc = "The lean runtime base: system toolchain, user-space package managers (pip/npm/mise/uv/bun), no-root chowned-rootfs model."
	}
	content, err := assets.RuntimeImageTemplatesFS.ReadFile(file)
	if err != nil {
		s.log.Warn("stock image template read failed", "file", file, "error", err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("stock image template unavailable"))
	}
	return connect.NewResponse(&apiv1.GetStockImageTemplateResponse{
		Tag:         tag,
		Name:        name,
		Description: desc,
		Dockerfile:  string(content),
	}), nil
}

// generatedDockerfile builds the image Dockerfile from the structured
// spec. The base FROM line is emitted by the service; the daemon
// RE-writes it to its own base anyway (defense in depth), and injects
// the runtime-base label. The root/chown wrapper keeps the rootfs
// writable by the non-root runtime user.
func generatedDockerfile(row db.RuntimeImageRow, base string) string {
	if strings.TrimSpace(row.DockerfileOverride) != "" {
		return row.DockerfileOverride
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "FROM %s\n", base)
	sb.WriteString("USER root\n")
	var apt []string
	_ = json.Unmarshal(row.AptPackages, &apt)
	if len(apt) > 0 {
		fmt.Fprintf(&sb, "RUN apt-get update && apt-get install -y --no-install-recommends \\\n")
		for _, p := range apt {
			fmt.Fprintf(&sb, "      %s \\\n", p)
		}
		sb.WriteString("      && rm -rf /var/lib/apt/lists/*\n")
	}
	var toolchains []string
	_ = json.Unmarshal(row.Toolchains, &toolchains)
	for _, t := range toolchains {
		fmt.Fprintf(&sb, "RUN %s\n", t)
	}
	var envMap map[string]string
	_ = json.Unmarshal(row.Env, &envMap)
	for k, v := range envMap {
		fmt.Fprintf(&sb, "ENV %s=%s\n", k, v)
	}
	sb.WriteString("RUN chown -R 1000:1000 /usr /opt /var 2>/dev/null || true\n")
	sb.WriteString("USER orchicon\n")
	sb.WriteString("WORKDIR /home/orchicon\n")
	return sb.String()
}

func toProto(r db.RuntimeImageRow) *apiv1.RuntimeImage {
	return &apiv1.RuntimeImage{
		Id:                 r.ID,
		TenantId:           r.TenantID,
		Name:               r.Name,
		Slug:               r.Slug,
		Description:        r.Description,
		BaseImageRef:       r.BaseImageRef,
		AptPackages:        string(r.AptPackages),
		Toolchains:         string(r.Toolchains),
		Env:                string(r.Env),
		DockerfileOverride: r.DockerfileOverride,
		Tag:                r.Tag,
		Status:             imageStatusProto(r.Status),
		BuildLog:           r.BuildLog,
		Error:              r.Error,
		FailureReason:      r.FailureReason,
		FailedStep:         r.FailedStep,
		LogTail:            r.LogTail,
		FailureCategory:    r.FailureCategory,
		Version:            int32(r.Version),
		BuiltVersion:       int32(r.BuiltVersion),
		CreatedAt:          timestamppb.New(r.CreatedAt),
		UpdatedAt:          timestamppb.New(r.UpdatedAt),
		Source:             r.Source,
	}
}

func imageStatusProto(status string) apiv1.RuntimeImageStatus {
	switch status {
	case "draft":
		return apiv1.RuntimeImageStatus_RUNTIME_IMAGE_STATUS_DRAFT
	case "building":
		return apiv1.RuntimeImageStatus_RUNTIME_IMAGE_STATUS_BUILDING
	case "ready":
		return apiv1.RuntimeImageStatus_RUNTIME_IMAGE_STATUS_READY
	case "failed":
		return apiv1.RuntimeImageStatus_RUNTIME_IMAGE_STATUS_FAILED
	case RuntimeImageStatusAbsent:
		// The spec is correct but its image is not downloaded locally and no
		// build is running — distinct from draft (never built) and failed (a
		// build was attempted and failed). Without this case the seeder's new
		// state would surface in the UI as UNSPECIFIED.
		return apiv1.RuntimeImageStatus_RUNTIME_IMAGE_STATUS_ABSENT
	}
	return apiv1.RuntimeImageStatus_RUNTIME_IMAGE_STATUS_UNSPECIFIED
}

func validateName(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", connect.NewError(connect.CodeInvalidArgument, errors.New("name required"))
	}
	if len(s) > maxNameLen {
		return "", connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("name too long (max %d)", maxNameLen))
	}
	return s, nil
}

func validateSlug(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", connect.NewError(connect.CodeInvalidArgument, errors.New("slug required"))
	}
	if !regexpSlug.MatchString(s) {
		return "", connect.NewError(connect.CodeInvalidArgument, errors.New("slug must be lowercase words separated by single hyphens"))
	}
	if len(s) > maxSlugLen {
		return "", connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("slug too long (max %d)", maxSlugLen))
	}
	return s, nil
}

func validateJSONArray(s, field string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return []byte("[]"), nil
	}
	var a []string
	if err := json.Unmarshal([]byte(s), &a); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%s must be a JSON array of strings", field))
	}
	for _, e := range a {
		if len(e) > 200 {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%s entry too long", field))
		}
	}
	return []byte(s), nil
}

func validateJSONObject(s, field string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return []byte("{}"), nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%s must be a JSON object of string values", field))
	}
	return []byte(s), nil
}

func bound(s string, max int) (string, error) {
	if len(s) > max {
		return "", connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("value too long (max %d)", max))
	}
	return s, nil
}

func isStreamDropped(err error) bool {
	if err == nil {
		return false
	}
	if e, ok := err.(*runtime.StreamDroppedError); ok && e != nil {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "stream") && (strings.Contains(msg, "disconnected") || strings.Contains(msg, "input stream") || strings.Contains(msg, "without exit event"))
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// --- audit helpers ---------------------------------------------------------

// recordAudit writes an actor-based audit row in the caller's tx,
// resolving the actor from the request context. Must be called in the
// same transaction as the mutation so the row commits atomically.
func recordAudit(ctx context.Context, tx pgx.Tx, tenantID, action, targetType, targetID string, before, after json.RawMessage) error {
	e := auth.ActorFromContext(ctx)
	if e.TenantID == "" {
		e.TenantID = tenantID
	}
	e.Action = action
	e.TargetType = targetType
	e.TargetID = targetID
	e.Before = before
	e.After = after
	return audit.Record(ctx, tx, e)
}

// runtimeImageAuditSnapshot is the non-secret projection of a runtime
// image spec for the audit trail. Env and the dockerfile override can
// embed secrets; apt_packages/toolchains are install commands (also
// potentially sensitive) — keep the snapshot to identity + status.
func runtimeImageAuditSnapshot(r db.RuntimeImageRow) map[string]any {
	return map[string]any{
		"id":      r.ID,
		"name":    r.Name,
		"slug":    r.Slug,
		"tag":     r.Tag,
		"status":  r.Status,
		"version": r.Version,
	}
}

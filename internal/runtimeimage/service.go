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
	"log/slog"
	"regexp"
	"strings"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	apiv1connect 	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	assets "github.com/beardedparrott/orchicon"
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
	pool   *db.Pool
	log    *slog.Logger
	rt     *runtime.Client // nil in headless (no daemon)
	base   string          // resolved base image ref from the daemon (cached at first Images call)
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
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create runtime image: %w", err))
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
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// Remove the local docker image (best-effort — the daemon may be
	// absent or the image may already be gone).
	if s.rt != nil && row.Tag != "" {
		if err := s.rt.RemoveImage(context.Background(), row.Tag); err != nil {
			s.log.Warn("runtime image docker rmi failed", "tag", row.Tag, "error", err)
		}
	}
	return connect.NewResponse(&apiv1.DeleteRuntimeImageResponse{}), nil
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
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	row, err := db.GetRuntimeImage(ctx, ttx.Tx, tenantID, req.Msg.Id)
	_ = ttx.Rollback(ctx)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return connect.NewError(connect.CodeNotFound, errors.New("runtime image not found"))
		}
		return connect.NewError(connect.CodeInternal, err)
	}
	if row.Status == "building" {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("image is already building"))
	}

	// Mark building + clear the old log/error.
	building := "building"
	clearErr := ""
	ttx, err = s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	row, err = db.UpdateRuntimeImage(ctx, ttx.Tx, tenantID, row.ID, row.Version, db.UpdateRuntimeImageFields{
		Status:   &building,
		Error:    &clearErr,
	})
	if err != nil {
		_ = ttx.Rollback(ctx)
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("optimistic concurrency conflict — reload and retry"))
	}
	if err := ttx.Commit(ctx); err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}

	base := row.BaseImageRef
	if base == "" {
		base = s.ensureBase(ctx)
	}
	if base == "" {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("no base image configured"))
	}
	df := generatedDockerfile(row, base)

	var logBuf strings.Builder
	exit, err := s.rt.BuildImage(ctx, runtime.BuildRequest{
		Slug:       row.Slug,
		Tag:        row.Tag,
		Base:       base,
		Dockerfile: df,
	}, func(ev runtime.AgentEvent) error {
		if ev.Data != "" {
			logBuf.WriteString(ev.Data)
			logBuf.WriteString("\n")
			return stream.Send(&apiv1.BuildRuntimeImageResponse{Log: ev.Data})
		}
		return nil
	})
	if err != nil {
		exit = 1
	}

	// Persist the outcome.
	status := "ready"
	buildLog := truncate(logBuf.String(), maxBuildLogLen)
	failMsg := ""
	if exit != 0 {
		status = "failed"
		if err != nil {
			failMsg = err.Error()
		}
		if failMsg == "" {
			failMsg = "docker build failed"
		}
	}
	ttx, err = s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	final, err := db.UpdateRuntimeImage(ctx, ttx.Tx, tenantID, row.ID, row.Version, db.UpdateRuntimeImageFields{
		Status:   &status,
		BuildLog: &buildLog,
		Error:    &failMsg,
	})
	if err != nil {
		_ = ttx.Rollback(ctx)
		s.log.Warn("persist runtime image build outcome failed", "id", row.ID, "error", err)
		// The build itself succeeded on the daemon; report the persisted
		// error so the caller can retry the status update.
		return connect.NewError(connect.CodeInternal, fmt.Errorf("persist build outcome: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	if err := stream.Send(&apiv1.BuildRuntimeImageResponse{
		Status: imageStatusProto(final.Status),
		Error:  final.Error,
		Tag:    final.Tag,
	}); err != nil {
		return err
	}
	if exit != 0 {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("image build failed: %s", final.Error))
	}
	return nil
}

// ListAvailableRuntimeImages returns the merged dropdown list for the
// work-item runtime_image field: the daemon's stock images plus the
// tenant's ready custom images, with the base image as the default.
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
	resp.CustomImages = tags
	if resp.DefaultImage == "" && len(resp.StockImages) > 0 {
		resp.DefaultImage = resp.StockImages[0]
	}
	return connect.NewResponse(resp), nil
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
		desc = "Go, Node, buf, atlas, and a baked PostgreSQL 15 for building and DB-testing the Orchicon repo in-sandbox (dogfooding)."
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
		Version:            int32(r.Version),
		CreatedAt:          timestamppb.New(r.CreatedAt),
		UpdatedAt:          timestamppb.New(r.UpdatedAt),
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

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

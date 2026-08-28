package askorchicon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"connectrpc.com/connect"
	"github.com/beardedparrott/orchicon/internal/audit"
	"github.com/beardedparrott/orchicon/internal/auth"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/runtime"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"github.com/jackc/pgx/v5"
)

const (
	riMaxNameLen        = 200
	riMaxSlugLen        = 64
	riMaxDescriptionLen = 4000
	riMaxDockerfileLen  = 1 << 16
	riMaxTagLen         = 256
	riMaxBuildLogLen    = 1 << 20
)

var riSlugRe = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

var toolRuntimeClient *runtime.Client

func toolListRuntimeImages(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	rows, err := db.ListRuntimeImages(ctx, ttx.Tx, db.RuntimeImageFilter{TenantID: tenantID})
	if err != nil {
		return nil, err
	}
	if rows == nil {
		return json.RawMessage("[]"), nil
	}
	return json.Marshal(rows)
}

func toolGetRuntimeImage(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	row, err := db.GetRuntimeImage(ctx, ttx.Tx, tenantID, params.ID)
	if err != nil {
		return nil, err
	}
	return json.Marshal(row)
}

func toolCreateRuntimeImage(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		Name               string   `json:"name"`
		Slug               string   `json:"slug"`
		Description        string   `json:"description"`
		AptPackages        []string `json:"apt_packages"`
		Toolchains         []string `json:"toolchains"`
		Env                string   `json:"env"`
		DockerfileOverride string   `json:"dockerfile_override"`
		Tag                string   `json:"tag"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	name, err := riValidateName(params.Name)
	if err != nil {
		return nil, err
	}
	slug, err := riValidateSlug(params.Slug)
	if err != nil {
		return nil, err
	}
	desc, err := riBound(params.Description, riMaxDescriptionLen)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	for _, e := range params.AptPackages {
		if len(e) > 200 {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("apt_packages entry too long"))
		}
	}
	for _, e := range params.Toolchains {
		if len(e) > 200 {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("toolchains entry too long"))
		}
	}
	aptJSON, _ := json.Marshal(params.AptPackages)
	if aptJSON == nil {
		aptJSON = []byte("[]")
	}
	toolJSON, _ := json.Marshal(params.Toolchains)
	if toolJSON == nil {
		toolJSON = []byte("[]")
	}
	envJSON, err := riValidateJSONObject(params.Env, "env")
	if err != nil {
		return nil, err
	}
	override, err := riBound(params.DockerfileOverride, riMaxDockerfileLen)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	tag := strings.TrimSpace(params.Tag)
	if tag == "" {
		tag = slug + ":latest"
	}
	if _, err := riBound(tag, riMaxTagLen); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	base := ""
	if toolRuntimeClient != nil {
		if imgs, err := toolRuntimeClient.Images(ctx); err == nil && imgs != nil {
			base = imgs.Default
		}
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	row, err := db.CreateRuntimeImage(ctx, ttx.Tx, db.RuntimeImageRow{
		ID:                 db.NewID(),
		TenantID:           tenantID,
		Name:               name,
		Slug:               slug,
		Description:        desc,
		BaseImageRef:       base,
		AptPackages:        aptJSON,
		Toolchains:         toolJSON,
		Env:                envJSON,
		DockerfileOverride: override,
		Tag:                tag,
		Status:             "draft",
		Source:             "custom",
	})
	if err != nil {
		return nil, err
	}
	if err := riRecordAudit(ctx, ttx.Tx, tenantID, "runtime_image.created", "runtime_image", row.ID, nil, audit.Snapshot(riAuditSnapshot(row))); err != nil {
		return nil, fmt.Errorf("audit runtime_image.created: %w", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(row)
}

func toolUpdateRuntimeImage(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(args, &raw); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	var id string
	var version int
	var name, slug, description, env, dockerfileOverride, tag *string
	var aptPackages, toolchains []string
	var hasApt, hasToolchains bool
	if v, ok := raw["id"]; ok {
		_ = json.Unmarshal(v, &id)
	}
	if v, ok := raw["version"]; ok {
		_ = json.Unmarshal(v, &version)
	}
	if v, ok := raw["name"]; ok {
		var s string
		_ = json.Unmarshal(v, &s)
		name = &s
	}
	if v, ok := raw["slug"]; ok {
		var s string
		_ = json.Unmarshal(v, &s)
		slug = &s
	}
	if v, ok := raw["description"]; ok {
		var s string
		_ = json.Unmarshal(v, &s)
		description = &s
	}
	if v, ok := raw["apt_packages"]; ok {
		_ = json.Unmarshal(v, &aptPackages)
		hasApt = true
	}
	if v, ok := raw["toolchains"]; ok {
		_ = json.Unmarshal(v, &toolchains)
		hasToolchains = true
	}
	if v, ok := raw["env"]; ok {
		var s string
		_ = json.Unmarshal(v, &s)
		env = &s
	}
	if v, ok := raw["dockerfile_override"]; ok {
		var s string
		_ = json.Unmarshal(v, &s)
		dockerfileOverride = &s
	}
	if v, ok := raw["tag"]; ok {
		var s string
		_ = json.Unmarshal(v, &s)
		tag = &s
	}
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("id is required")
	}
	if _, ok := raw["version"]; !ok {
		return nil, fmt.Errorf("version is required for optimistic concurrency")
	}
	f := db.UpdateRuntimeImageFields{}
	if name != nil {
		v, err := riValidateName(*name)
		if err != nil {
			return nil, err
		}
		f.Name = &v
	}
	if slug != nil {
		v, err := riValidateSlug(*slug)
		if err != nil {
			return nil, err
		}
		f.Slug = &v
	}
	if description != nil {
		v, err := riBound(*description, riMaxDescriptionLen)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		f.Description = &v
	}
	if hasApt {
		for _, e := range aptPackages {
			if len(e) > 200 {
				return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("apt_packages entry too long"))
			}
		}
		b, _ := json.Marshal(aptPackages)
		if b == nil {
			b = []byte("[]")
		}
		f.AptPackages = &b
	}
	if hasToolchains {
		for _, e := range toolchains {
			if len(e) > 200 {
				return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("toolchains entry too long"))
			}
		}
		b, _ := json.Marshal(toolchains)
		if b == nil {
			b = []byte("[]")
		}
		f.Toolchains = &b
	}
	if env != nil {
		v, err := riValidateJSONObject(*env, "env")
		if err != nil {
			return nil, err
		}
		f.Env = &v
	}
	if dockerfileOverride != nil {
		v, err := riBound(*dockerfileOverride, riMaxDockerfileLen)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		f.DockerfileOverride = &v
	}
	if tag != nil {
		v, err := riBound(*tag, riMaxTagLen)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		trim := strings.TrimSpace(v)
		if trim != "" {
			f.Tag = &trim
		}
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	cur, err := db.GetRuntimeImage(ctx, ttx.Tx, tenantID, id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("runtime image not found"))
		}
		return nil, err
	}
	if cur.Status == "building" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("cannot edit an image while it is building"))
	}
	if cur.Status != "draft" {
		status := "draft"
		f.Status = &status
	}
	row, err := db.UpdateRuntimeImage(ctx, ttx.Tx, tenantID, id, version, f)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("optimistic concurrency conflict — reload and retry"))
		}
		return nil, err
	}
	if err := riRecordAudit(ctx, ttx.Tx, tenantID, "runtime_image.updated", "runtime_image", row.ID, audit.Snapshot(riAuditSnapshot(cur)), audit.Snapshot(riAuditSnapshot(row))); err != nil {
		return nil, fmt.Errorf("audit runtime_image.updated: %w", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(row)
}

func toolBuildRuntimeImage(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ID      string `json:"id"`
		Version int    `json:"version"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if strings.TrimSpace(params.ID) == "" {
		return nil, fmt.Errorf("id is required")
	}
	if params.Version == 0 {
		return nil, fmt.Errorf("version is required for optimistic concurrency")
	}
	if toolRuntimeClient == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("runtime daemon not configured (headless serve)"))
	}
	tenantID := tenant.FromContext(ctx)
	if tenantID == "" {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("tenant required"))
	}
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	row, err := db.GetRuntimeImageForUpdate(ctx, ttx.Tx, tenantID, params.ID)
	if err != nil {
		_ = ttx.Rollback(ctx)
		if errors.Is(err, db.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("runtime image not found"))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if params.Version != row.Version {
		_ = ttx.Rollback(ctx)
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("optimistic concurrency conflict — reload and retry"))
	}
	if row.Status == "building" {
		_ = ttx.Rollback(ctx)
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("image is already building"))
	}
	if row.Status == "ready" && row.Tag != "" && row.BuiltVersion == row.Version {
		_ = ttx.Rollback(ctx)
		out := map[string]any{
			"id":            row.ID,
			"tag":           row.Tag,
			"status":        row.Status,
			"error":         row.Error,
			"skipped":       true,
			"logs":          fmt.Sprintf("runtime image already up to date (spec version %d) — skipping rebuild", row.Version),
			"version":       row.Version,
			"built_version": row.BuiltVersion,
		}
		return json.Marshal(out)
	}
	building := "building"
	clearErr := ""
	row, err = db.UpdateRuntimeImage(ctx, ttx.Tx, tenantID, row.ID, row.Version, db.UpdateRuntimeImageFields{
		Status:     &building,
		Error:      &clearErr,
		StatusOnly: true,
	})
	if err != nil {
		_ = ttx.Rollback(ctx)
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("optimistic concurrency conflict — reload and retry"))
	}
	if err := riRecordAudit(ctx, ttx.Tx, tenantID, "runtime_image.build_started", "runtime_image", row.ID, nil, audit.Snapshot(map[string]any{"tag": row.Tag, "status": "building"})); err != nil {
		_ = ttx.Rollback(ctx)
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit runtime_image.build_started: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	base := row.BaseImageRef
	if base == "" {
		if imgs, err := toolRuntimeClient.Images(ctx); err == nil && imgs != nil {
			base = imgs.Default
		}
	}
	if base == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("no base image configured"))
	}
	df := riGeneratedDockerfile(row, base)
	var logBuf strings.Builder
	exit, berr := toolRuntimeClient.BuildImage(ctx, runtime.BuildRequest{
		Slug:        row.Slug,
		Tag:         row.Tag,
		Base:        base,
		Dockerfile:  df,
		SpecVersion: row.Version,
	}, func(ev runtime.AgentEvent) error {
		if ev.Data != "" {
			logBuf.WriteString(ev.Data)
			logBuf.WriteString("\n")
		}
		return nil
	})
	if berr != nil {
		exit = 1
	}
	status := "ready"
	buildLog := riTruncate(logBuf.String(), riMaxBuildLogLen)
	failMsg := ""
	if exit != 0 {
		status = "failed"
		if berr != nil {
			failMsg = berr.Error()
		}
		if failMsg == "" {
			failMsg = "docker build failed"
		}
	}
	ttx, err = pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	fields := db.UpdateRuntimeImageFields{
		Status:     &status,
		BuildLog:   &buildLog,
		Error:      &failMsg,
		StatusOnly: true,
	}
	if exit == 0 {
		built := row.Version
		fields.BuiltVersion = &built
	}
	final, err := db.UpdateRuntimeImage(ctx, ttx.Tx, tenantID, row.ID, row.Version, fields)
	if err != nil {
		_ = ttx.Rollback(ctx)
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("persist build outcome: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := map[string]any{
		"id":            final.ID,
		"tag":           final.Tag,
		"status":        final.Status,
		"error":         final.Error,
		"skipped":       false,
		"logs":          buildLog,
		"version":       final.Version,
		"built_version": final.BuiltVersion,
	}
	return json.Marshal(out)
}

func toolDeleteRuntimeImage(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	row, err := db.GetRuntimeImage(ctx, ttx.Tx, tenantID, params.ID)
	if err != nil {
		return nil, err
	}
	if err := db.DeleteRuntimeImage(ctx, ttx.Tx, tenantID, params.ID); err != nil {
		return nil, err
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(row)
}

func riValidateName(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", connect.NewError(connect.CodeInvalidArgument, errors.New("name required"))
	}
	if len(s) > riMaxNameLen {
		return "", connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("name too long (max %d)", riMaxNameLen))
	}
	return s, nil
}

func riValidateSlug(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", connect.NewError(connect.CodeInvalidArgument, errors.New("slug required"))
	}
	if !riSlugRe.MatchString(s) {
		return "", connect.NewError(connect.CodeInvalidArgument, errors.New("slug must be lowercase words separated by single hyphens"))
	}
	if len(s) > riMaxSlugLen {
		return "", connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("slug too long (max %d)", riMaxSlugLen))
	}
	return s, nil
}

func riValidateJSONObject(s, field string) ([]byte, error) {
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

func riBound(s string, max int) (string, error) {
	if len(s) > max {
		return "", fmt.Errorf("value too long (max %d)", max)
	}
	return s, nil
}

func riTruncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

func riGeneratedDockerfile(row db.RuntimeImageRow, base string) string {
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

func riRecordAudit(ctx context.Context, tx pgx.Tx, tenantID, action, targetType, targetID string, before, after json.RawMessage) error {
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

func riAuditSnapshot(r db.RuntimeImageRow) map[string]any {
	return map[string]any{
		"id":      r.ID,
		"name":    r.Name,
		"slug":    r.Slug,
		"tag":     r.Tag,
		"status":  r.Status,
		"version": r.Version,
	}
}

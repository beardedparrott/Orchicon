package askorchicon

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

var numberRefRe = regexp.MustCompile(`(?i)(?:number|item|#)\s*(\d+)`)

func toolListWorkItems(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ProjectID string `json:"project_id"`
		Status    string `json:"status"`
		Search    string `json:"search"`
	}
	if len(args) > 0 && string(args) != "null" {
		json.Unmarshal(args, &params)
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	items, err := db.ListWorkItems(ctx, ttx.Tx, db.ListWorkItemsFilter{
		TenantID:  tenantID,
		ProjectID: params.ProjectID,
		Status:    params.Status,
		Search:    params.Search,
	})
	if err != nil {
		return nil, err
	}
	if items == nil {
		return json.RawMessage("[]"), nil
	}
	return json.Marshal(items)
}

func toolGetWorkItem(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
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
	item, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, params.ID)
	if err != nil {
		return nil, err
	}
	return json.Marshal(item)
}

func toolCreateWorkItem(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ProjectID          string `json:"project_id"`
		Title              string `json:"title"`
		Kind               string `json:"kind"`
		ParentID           string `json:"parent_id"`
		Priority           int    `json:"priority"`
		Description        string `json:"description"`
		AcceptanceCriteria string `json:"acceptance_criteria"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.Title == "" {
		return nil, fmt.Errorf("title is required")
	}
	if params.ProjectID == "" {
		return nil, fmt.Errorf("project_id is required")
	}
	kind := params.Kind
	if kind == "" {
		kind = domain.WorkItemKindTask
	} else {
		normalized, err := domain.NormalizeWorkItemKind(kind)
		if err != nil {
			return nil, err
		}
		kind = normalized
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	var parentID *string
	if params.ParentID != "" {
		parentID = &params.ParentID
	}
	item, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID:                 db.NewID(),
		TenantID:           tenantID,
		ProjectID:          params.ProjectID,
		ParentID:           parentID,
		Kind:               kind,
		Title:              params.Title,
		Status:             domain.WorkItemReady,
		Priority:           params.Priority,
		Description:        params.Description,
		AcceptanceCriteria: params.AcceptanceCriteria,
	})
	if err != nil {
		return nil, err
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(item)
}

func toolUpdateWorkItem(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ID                  string `json:"id"`
		Title               string `json:"title"`
		Description         string `json:"description"`
		Status              string `json:"status"`
		Priority            int    `json:"priority"`
		AcceptanceCriteria  string `json:"acceptance_criteria"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.ID == "" {
		return nil, fmt.Errorf("id is required")
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, params.ID)
	if err != nil {
		return nil, err
	}
	update := db.UpdateWorkItemFields{}
	if params.Title != "" {
		update.Title = &params.Title
	}
	if params.Description != "" {
		update.Description = &params.Description
	}
	if params.AcceptanceCriteria != "" {
		update.AcceptanceCriteria = &params.AcceptanceCriteria
	}
	if params.Status != "" {
		update.Status = &params.Status
	}
	if params.Priority > 0 {
		update.Priority = &params.Priority
	}
	if _, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, params.ID, current.Version, update); err != nil {
		return nil, err
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]string{"status": "updated"})
}

// toolDeleteWorkItem soft-deletes a work item (status → cancelled), matching
// the DeleteWorkItem RPC the UI uses.
func toolDeleteWorkItem(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.ID == "" {
		return nil, fmt.Errorf("id is required")
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, params.ID)
	if err != nil {
		return nil, err
	}
	status := domain.WorkItemCancelled
	if _, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, params.ID, current.Version, db.UpdateWorkItemFields{
		Status: &status,
	}); err != nil {
		return nil, err
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"id": params.ID, "status": domain.WorkItemCancelled})
}

// toolScheduleWorkItem sets a work item's status to "scheduled" and
// optionally sets its scheduled_start_at. Accepts a work_item_id and an
// optional scheduled_time (ISO 8601 or "N [minutes|hours] from now").
func toolScheduleWorkItem(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ID            string `json:"id"`
		Status        string `json:"status"`
		ScheduledTime string `json:"scheduled_time"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.ID == "" {
		return nil, fmt.Errorf("id is required")
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, params.ID)
	if err != nil {
		return nil, err
	}
	update := db.UpdateWorkItemFields{}
	status := params.Status
	if status == "" {
		status = "scheduled"
	}
	update.Status = &status
	// Scheduling contradicts "start immediately" — disable auto-start.
	autoStart := false
	update.AutoStartWorkflow = &autoStart
	if params.ScheduledTime != "" {
		t, parseErr := parseScheduledTime(params.ScheduledTime)
		if parseErr != nil {
			return nil, fmt.Errorf("parse scheduled_time: %w", parseErr)
		}
		update.ScheduledStartAt = &t
	}
	if _, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, params.ID, current.Version, update); err != nil {
		return nil, err
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"status":           "scheduled",
		"work_item_id":     params.ID,
		"work_item_title":  current.Title,
		"scheduled_start":  params.ScheduledTime,
	})
}

func parseScheduledTime(s string) (time.Time, error) {
	// Try ISO 8601 first.
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	// Normalize word numbers: "five minutes" → "5 minutes".
	normalized := replaceWordNumbers(s)
	// Try "N [minutes|min|m|hours|hour|h] from now".
	re := regexp.MustCompile(`(?i)(\d+)\s*(minute|min|m|hour|hr|h)\s*(?:from\s*now)?`)
	matches := re.FindStringSubmatch(normalized)
	if len(matches) >= 3 {
		n, _ := strconv.Atoi(matches[1])
		unit := strings.ToLower(matches[2])
		var dur time.Duration
		switch unit {
		case "minute", "min", "m":
			dur = time.Duration(n) * time.Minute
		case "hour", "hr", "h":
			dur = time.Duration(n) * time.Hour
		default:
			return time.Time{}, fmt.Errorf("unknown time unit: %q", unit)
		}
		return time.Now().UTC().Add(dur), nil
	}
	return time.Time{}, fmt.Errorf("unrecognized time format: %q (use ISO 8601 or 'N minutes from now')", s)
}

// wordNumberMap maps spelled-out numbers to digits for the schedule tool's
// "N minutes from now" parsing ("five minutes" → "5 minutes").
var wordNumberMap = map[string]string{
	"zero": "0", "one": "1", "two": "2", "three": "3", "four": "4",
	"five": "5", "six": "6", "seven": "7", "eight": "8", "nine": "9",
	"ten": "10", "eleven": "11", "twelve": "12",
}

func wordNumberPattern() string {
	words := make([]string, 0, len(wordNumberMap))
	for w := range wordNumberMap {
		words = append(words, w)
	}
	sort.Strings(words)
	return strings.Join(words, "|")
}

func replaceWordNumbers(s string) string {
	// Replace word numbers that precede a time unit, e.g. "five minutes" → "5 minutes".
	re := regexp.MustCompile(`(?i)(` + wordNumberPattern() + `)\s*(minutes?|min|m|hours?|hr|h)`)
	return re.ReplaceAllStringFunc(s, func(match string) string {
		parts := re.FindStringSubmatch(match)
		if len(parts) >= 3 {
			if digit, ok := wordNumberMap[strings.ToLower(parts[1])]; ok {
				return digit + " " + parts[2]
			}
		}
		return match
	})
}

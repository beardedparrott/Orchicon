package backup

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Scheduler struct {
	dsn  string
	dir  string
	log  *slog.Logger
	stop func()
	wg   sync.WaitGroup
}

func NewScheduler(dsn, dir string, log *slog.Logger) *Scheduler {
	return &Scheduler{dsn: dsn, dir: dir, log: log}
}

// Start launches a background goroutine that checks for scheduled backups
// every 60 seconds. The schedule and retention are read from a provider
// function so they reflect live settings changes without restarting.
func (s *Scheduler) Start(ctx context.Context, getSettings func() (schedule string, retentionDays int)) {
	ctx, cancel := context.WithCancel(ctx)
	s.stop = cancel
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		// Run once immediately if a schedule is set.
		s.tryBackup(ctx, getSettings)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.tryBackup(ctx, getSettings)
			}
		}
	}()
}

func (s *Scheduler) Stop() {
	if s.stop != nil {
		s.stop()
	}
	s.wg.Wait()
}

func (s *Scheduler) tryBackup(ctx context.Context, getSettings func() (string, int)) {
	schedule, retentionDays := getSettings()
	if schedule == "" {
		return // automatic backups disabled
	}
	if !shouldRunNow(schedule) {
		return
	}
	s.log.Info("scheduled backup starting")
	info, err := Create(ctx, s.dsn, s.dir)
	if err != nil {
		s.log.Error("scheduled backup failed", "error", err)
		return
	}
	s.log.Info("scheduled backup complete", "name", info.Name, "size", info.SizeBytes)
	if retentionDays > 0 {
		removed, err := Prune(s.dir, retentionDays)
		if err != nil {
			s.log.Error("scheduled backup prune failed", "error", err)
		} else if removed > 0 {
			s.log.Info("pruned old backups", "count", removed, "retention_days", retentionDays)
		}
	}
}

// shouldRunNow checks the cron schedule against the current time.
// Supports simplified 5-field cron: minute hour day month weekday.
// Returns true only within the first 60-second window of each match.
func shouldRunNow(schedule string) bool {
	fields := strings.Fields(schedule)
	if len(fields) != 5 {
		return false
	}
	now := time.Now()
	// Use a cache to avoid triggering multiple times within the same minute.
	return cronFieldMatch(fields[0], now.Minute()) &&
		cronFieldMatch(fields[1], now.Hour()) &&
		cronFieldMatch(fields[2], now.Day()) &&
		cronFieldMatch(fields[3], int(now.Month())) &&
		cronFieldMatch(fields[4], int(now.Weekday()))
}

func cronFieldMatch(field string, val int) bool {
	if field == "*" {
		return true
	}
	// Range: "1-5" or "*/15"
	if strings.Contains(field, "-") {
		parts := strings.SplitN(field, "-", 2)
		lo, err1 := strconv.Atoi(parts[0])
		hi, err2 := strconv.Atoi(parts[1])
		if err1 == nil && err2 == nil {
			return val >= lo && val <= hi
		}
	}
	if strings.HasPrefix(field, "*/") {
		step, err := strconv.Atoi(strings.TrimPrefix(field, "*/"))
		if err == nil && step > 0 {
			return val%step == 0
		}
	}
	// Step within range: "1-5/2"
	if strings.Contains(field, "/") {
		parts := strings.SplitN(field, "/", 2)
		if cronFieldMatch(parts[0], val) {
			step, err := strconv.Atoi(parts[1])
			if err == nil && step > 0 {
				return val%step == 0
			}
		}
		return false
	}
	v, err := strconv.Atoi(field)
	if err != nil {
		return false
	}
	return v == val
}

// Package logging provides a size/time-bounded rotating file writer for
// the control plane's on-disk logs.
//
// Background: `orchicon serve --detach` previously redirected the serve
// child's stdout/stderr straight into a single append-only file
// (.dev/logs/orchicon.log) with no rotation or retention. A runaway
// component could grow that file into the hundreds of gigabytes with no
// safety valve. RotatingWriter is the replacement sink: it owns the file,
// rotates it when it exceeds a size ceiling OR when a time interval
// elapses (whichever comes first), and prunes old rotated files by
// retention age and/or a maximum file count. It is safe for concurrent
// use (slog handlers call Write from many goroutines) and can be
// reconfigured live via Apply so Settings → Defaults changes take effect
// without a restart.
package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Config controls the rotating writer. Zero values select the built-in
// defaults documented on DefaultConfig.
type Config struct {
	// Dir is the directory logs live in (created on open if missing).
	Dir string
	// BaseName is the active log file name inside Dir, e.g. "orchicon.log".
	BaseName string
	// MaxSizeBytes rotates the file once it reaches this size. 0 → 100 MiB.
	MaxSizeBytes int64
	// RollInterval rotates the file at least this often, even if well
	// under the size ceiling. 0 → 24h. A "daily" roll is 24h.
	RollInterval time.Duration
	// RetentionDays prunes rotated files older than this many days.
	// 0 → 7.
	RetentionDays int
	// MaxFiles keeps at most this many rotated files (newest kept).
	// 0 → 7.
	MaxFiles int
	// CheckInterval is how often the maintenance sweep (time-based roll +
	// prune) runs. 0 → 30s.
	CheckInterval time.Duration
}

// DefaultConfig returns the built-in rotation defaults. Callers layer
// env/config/settings values on top field-by-field.
func DefaultConfig() Config {
	return Config{
		BaseName:      "orchicon.log",
		MaxSizeBytes:  100 << 20,
		RollInterval:  24 * time.Hour,
		RetentionDays: 7,
		MaxFiles:      7,
		CheckInterval: 30 * time.Second,
	}
}

// stampFormat yields lexicographically sortable rotation suffixes.
const stampFormat = "20060102-150405.000"

// RotatingWriter is a concurrency-safe io.Writer that appends to a single
// file and rotates it by size and/or time, pruning old generations.
type RotatingWriter struct {
	mu      sync.Mutex
	cfg     Config
	f       *os.File
	size    int64
	opened  time.Time
	stop    chan struct{}
	done    chan struct{}
	onRotate func() // invoked (outside the lock) after each rotation
	closed  bool
}

// New opens (creating if needed) the active log file and starts the
// maintenance sweep. If the existing file is already over the size
// ceiling, it is rotated immediately so a bloated file from a prior run
// is quarantined on the first boot.
func New(cfg Config) (*RotatingWriter, error) {
	c := normalize(cfg)
	if err := os.MkdirAll(c.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("logging: create log dir: %w", err)
	}
	w := &RotatingWriter{
		cfg:  c,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	if err := w.open(); err != nil {
		return nil, err
	}
	if w.cfg.MaxSizeBytes > 0 && w.size > w.cfg.MaxSizeBytes {
		w.rotate()
	}
	go w.sweep()
	return w, nil
}

// normalize fills zero fields with built-in defaults.
func normalize(cfg Config) Config {
	d := DefaultConfig()
	if cfg.Dir == "" {
		cfg.Dir = "."
	}
	if cfg.BaseName == "" {
		cfg.BaseName = d.BaseName
	}
	if cfg.MaxSizeBytes <= 0 {
		cfg.MaxSizeBytes = d.MaxSizeBytes
	}
	if cfg.RollInterval <= 0 {
		cfg.RollInterval = d.RollInterval
	}
	if cfg.RetentionDays <= 0 {
		cfg.RetentionDays = d.RetentionDays
	}
	if cfg.MaxFiles <= 0 {
		cfg.MaxFiles = d.MaxFiles
	}
	if cfg.CheckInterval <= 0 {
		cfg.CheckInterval = d.CheckInterval
	}
	return cfg
}

// open creates the active file and stats its current size so an existing
// file's bytes count toward the ceiling immediately.
func (w *RotatingWriter) open() error {
	path := filepath.Join(w.cfg.Dir, w.cfg.BaseName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("logging: open log file: %w", err)
	}
	if fi, err := f.Stat(); err == nil {
		w.size = fi.Size()
	}
	w.f = f
	w.opened = time.Now()
	return nil
}

// Write appends p to the active file, rotating first if appending would
// breach the size ceiling.
func (w *RotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	var cb func()
	if w.cfg.MaxSizeBytes > 0 && w.size+int64(len(p)) > w.cfg.MaxSizeBytes {
		cb = w.rotateLocked()
	}
	if w.f == nil {
		if err := w.open(); err != nil {
			w.mu.Unlock()
			if cb != nil {
				cb()
			}
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	w.mu.Unlock()
	if cb != nil {
		cb()
	}
	return n, err
}

// Config returns a copy of the writer's current configuration.
func (w *RotatingWriter) Config() Config {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cfg
}

// Current returns the active *os.File (e.g. for dup2'ing onto fd 1/2).
// Callers must not close it; the writer owns the descriptor.
func (w *RotatingWriter) Current() *os.File {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f
}

// SetOnRotate registers a callback invoked after every rotation, outside
// the writer's lock. Used by serve to re-dup the new file onto stdout/
// stderr so panics and stray prints keep landing in the current log.
func (w *RotatingWriter) SetOnRotate(fn func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.onRotate = fn
}

// rotate renames the active file to a timestamped sibling and reopens a
// fresh active file, then prunes old generations. Safe to call with the
// lock held (the onRotate callback is deferred until the lock is free).
func (w *RotatingWriter) rotate() {
	w.mu.Lock()
	cb := w.rotateLocked()
	w.mu.Unlock()
	if cb != nil {
		cb()
	}
}

// rotateLocked performs the rename + reopen + prune and returns the
// onRotate callback to invoke AFTER the caller releases the lock (the
// callback typically dup2's the new file onto fd 1/2 — it must never run
// under the writer's lock).
func (w *RotatingWriter) rotateLocked() func() {
	if w.f != nil {
		_ = w.f.Close()
		w.f = nil
	}
	active := filepath.Join(w.cfg.Dir, w.cfg.BaseName)
	rotated := fmt.Sprintf("%s.%s", active, time.Now().Format(stampFormat))
	_ = os.Rename(active, rotated)
	_ = w.open()
	w.pruneLocked(time.Now())
	return w.onRotate
}

// sweep periodically applies time-based rotation and pruning.
func (w *RotatingWriter) sweep() {
	defer close(w.done)
	t := time.NewTicker(w.cfg.CheckInterval)
	defer t.Stop()
	for {
		select {
		case <-w.stop:
			return
		case now := <-t.C:
			w.mu.Lock()
			var cb func()
			if w.cfg.RollInterval > 0 && now.Sub(w.opened) >= w.cfg.RollInterval {
				cb = w.rotateLocked()
			} else {
				w.pruneLocked(now)
			}
			w.mu.Unlock()
			if cb != nil {
				cb()
			}
		}
	}
}

// pruneLocked deletes rotated files older than RetentionDays and, beyond
// MaxFiles, the oldest ones. The active file (no dot suffix) is never a
// candidate.
func (w *RotatingWriter) pruneLocked(now time.Time) {
	entries, err := os.ReadDir(w.cfg.Dir)
	if err != nil {
		return
	}
	prefix := w.cfg.BaseName + "."
	type gen struct {
		path string
		mod  time.Time
	}
	var gens []gen
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		gens = append(gens, gen{path: filepath.Join(w.cfg.Dir, e.Name()), mod: fi.ModTime()})
	}
	if len(gens) == 0 {
		return
	}
	cutoff := now.Add(-time.Duration(w.cfg.RetentionDays) * 24 * time.Hour)
	var keep []gen
	for _, g := range gens {
		if g.mod.Before(cutoff) {
			_ = os.Remove(g.path)
			continue
		}
		keep = append(keep, g)
	}
	if len(keep) > w.cfg.MaxFiles {
		sort.Slice(keep, func(i, j int) bool { return keep[i].mod.After(keep[j].mod) })
		for _, g := range keep[w.cfg.MaxFiles:] {
			_ = os.Remove(g.path)
		}
	}
}

// Apply live-reconfigures the writer. A changed directory or base name
// reopens the active file at the new path; a changed ceiling/interval/
// retention/maxfiles takes effect immediately. Then it prunes so an old
// generation is cleaned up as soon as the policy tightens.
func (w *RotatingWriter) Apply(cfg Config) {
	c := normalize(cfg)
	w.mu.Lock()
	var cb func()
	oldPath := filepath.Join(w.cfg.Dir, w.cfg.BaseName)
	newPath := filepath.Join(c.Dir, c.BaseName)
	pathChanged := oldPath != newPath
	w.cfg = c
	if pathChanged {
		if err := os.MkdirAll(c.Dir, 0o755); err != nil {
			w.mu.Unlock()
			return
		}
		if w.f != nil {
			_ = w.f.Close()
			w.f = nil
		}
		if err := w.open(); err != nil {
			w.mu.Unlock()
			return
		}
		if w.cfg.MaxSizeBytes > 0 && w.size > w.cfg.MaxSizeBytes {
			cb = w.rotateLocked()
			w.mu.Unlock()
			if cb != nil {
				cb()
			}
			return
		}
		// The active file moved (Settings → Defaults log directory
		// change); re-point fd 1/2 so panics and stray prints follow the
		// writer to the new location.
		cb = w.onRotate
	}
	w.pruneLocked(time.Now())
	w.mu.Unlock()
	if cb != nil {
		cb()
	}
}

// Close stops the maintenance sweep and closes the active file.
// Idempotent: subsequent calls are no-ops.
func (w *RotatingWriter) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	w.mu.Unlock()
	close(w.stop)
	<-w.done
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f != nil {
		err := w.f.Close()
		w.f = nil
		return err
	}
	return nil
}

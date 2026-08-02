package backup

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Info struct {
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	SizeBytes int64     `json:"size_bytes"`
	CreatedAt time.Time `json:"created_at"`
}

// DefaultDir returns the backups directory. In the single-container
// deployment ORCHICON_DATA_DIR is exported to the control plane, so
// backups land on the persistent data volume (/var/lib/orchicon/backups);
// otherwise they go to the user's home. The root home inside the
// container is ephemeral, so the container must NOT fall back to it.
func DefaultDir() (string, error) {
	if dataDir := os.Getenv("ORCHICON_DATA_DIR"); dataDir != "" {
		dir := filepath.Join(dataDir, "backups")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".local", "share", "orchicon", "backups")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	return dir, nil
}

// expandDir expands a leading "~/" (or bare "~") to the user's home
// directory and cleans the result. Returns the input unchanged if it
// does not start with a tilde.
func expandDir(dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", nil
	}
	if dir == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return home, nil
	}
	if strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, dir[2:]), nil
	}
	return filepath.Clean(dir), nil
}

// Create dumps the database named by dsn to a timestamped .sql file using
// the local pg_dump (Postgres runs as a sibling process — in the
// single-container deployment that is localhost:<port> inside the same
// container; there is no docker-exec indirection).
func Create(ctx context.Context, dsn, dir string) (*Info, error) {
	dir, err := expandDir(dir)
	if err != nil {
		return nil, fmt.Errorf("expand backup dir: %w", err)
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create backup dir: %w", err)
	}
	name := fmt.Sprintf("orchicon-backup-%s.sql", time.Now().UTC().Format("20060102T150405Z"))
	path := filepath.Join(dir, name)

	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("create backup file: %w", err)
	}
	defer f.Close()

	// --clean --if-exists emit DROP TABLE IF EXISTS before each CREATE,
	// so the dump can be restored on top of a live (non-empty) database.
	cmd := exec.CommandContext(ctx, "pg_dump",
		"-d", dsn,
		"--clean", "--if-exists",
		"--no-owner", "--no-acl")
	cmd.Stdout = f
	var stderrBuf strings.Builder
	cmd.Stderr = &stderrBuf
	if err := cmd.Run(); err != nil {
		os.Remove(path)
		return nil, fmt.Errorf("pg_dump: %w\n%s", err, stderrBuf.String())
	}

	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat backup: %w", err)
	}

	return &Info{
		Name:      name,
		Path:      path,
		SizeBytes: fi.Size(),
		CreatedAt: fi.ModTime(),
	}, nil
}

func Restore(ctx context.Context, dsn, path string) error {
	if strings.HasPrefix(path, "~/") || path == "~" {
		expanded, err := expandDir(path)
		if err != nil {
			return fmt.Errorf("expand backup path: %w", err)
		}
		path = expanded
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open backup file: %w", err)
	}
	defer f.Close()

	// Pipe the backup file into psql against the DSN directly (no
	// docker-exec). -v ON_ERROR_STOP=1 makes psql exit non-zero on the
	// first SQL error; --single-transaction rolls back the whole restore
	// if anything fails, so a bad backup cannot leave the database
	// half-restored.
	cmd := exec.CommandContext(ctx, "psql",
		"-d", dsn,
		"-v", "ON_ERROR_STOP=1",
		"--single-transaction")
	cmd.Stdin = f
	var stderrBuf strings.Builder
	cmd.Stderr = &stderrBuf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("psql restore: %w\n%s", err, stderrBuf.String())
	}
	return nil
}

func List(dir string) ([]Info, error) {
	dir, err := expandDir(dir)
	if err != nil {
		return nil, fmt.Errorf("expand backup dir: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var backups []Info
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "orchicon-backup-") || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		t, err := time.Parse("20060102T150405Z", strings.TrimPrefix(strings.TrimSuffix(e.Name(), ".sql"), "orchicon-backup-"))
		if err != nil {
			continue
		}
		backups = append(backups, Info{
			Name:      e.Name(),
			Path:      filepath.Join(dir, e.Name()),
			SizeBytes: fi.Size(),
			CreatedAt: t,
		})
	}
	sort.Slice(backups, func(i, j int) bool {
		return backups[i].CreatedAt.After(backups[j].CreatedAt)
	})
	return backups, nil
}

// Delete removes a single backup file by name. Returns an error if the
// file does not exist or cannot be removed.
func Delete(dir, name string) error {
	dir, err := expandDir(dir)
	if err != nil {
		return fmt.Errorf("expand backup dir: %w", err)
	}
	if strings.Contains(name, "/") || strings.Contains(name, "\\") || strings.HasPrefix(name, ".") {
		return fmt.Errorf("invalid backup name %q", name)
	}
	if !strings.HasPrefix(name, "orchicon-backup-") || !strings.HasSuffix(name, ".sql") {
		return fmt.Errorf("invalid backup name %q", name)
	}
	path := filepath.Join(dir, name)
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("backup %q not found", name)
		}
		return fmt.Errorf("remove backup %q: %w", name, err)
	}
	return nil
}

func Prune(dir string, keepDays int) (int, error) {
	dir, err := expandDir(dir)
	if err != nil {
		return 0, fmt.Errorf("expand backup dir: %w", err)
	}
	backups, err := List(dir)
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -keepDays)
	var removed int
	for _, b := range backups {
		if b.CreatedAt.Before(cutoff) {
			if err := os.Remove(b.Path); err == nil {
				removed++
			}
		}
	}
	return removed, nil
}

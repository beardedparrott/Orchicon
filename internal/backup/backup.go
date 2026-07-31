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

func DefaultDir() (string, error) {
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

// pgContainer returns the Docker container name for Postgres based on the
// DSN port. Dev uses 5432 (orchicon-postgres), prod uses 5433
// (orchicon-prod-postgres). Falls back to orchicon-postgres.
func pgContainer(dsn string) string {
	if strings.Contains(dsn, ":5433") {
		return "orchicon-prod-postgres"
	}
	return "orchicon-postgres"
}

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

	container := pgContainer(dsn)
	// Pipe pg_dump stdout directly to the host file. Avoids docker cp
	// which can have path parsing issues with bind-mounted volumes.
	// --clean --if-exists emit DROP TABLE IF EXISTS before each CREATE,
	// so the dump can be restored on top of a live (non-empty) database.
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", container,
		"pg_dump", "-U", "orchicon", "-d", "orchicon",
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

	container := pgContainer(dsn)
	// Pipe the backup file into psql inside the container.
	// -v ON_ERROR_STOP=1 makes psql exit non-zero on the first SQL error
	//   (by default it prints the error but still exits 0, silently
	//   "succeeding" a restore that did nothing). --single-transaction
	//   rolls back the whole restore if anything fails, so a bad backup
	//   cannot leave the database half-restored.
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", container,
		"psql", "-U", "orchicon", "-d", "orchicon",
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

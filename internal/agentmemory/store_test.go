package agentmemory

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func openTest(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir
}

func writeT(t *testing.T, s *Store, tenant, dir, title, body string, tags []string) int64 {
	t.Helper()
	id, err := s.Write(context.Background(), WriteInput{TenantID: tenant, ProjectDir: dir, ExecutionID: "exec1", WorkerID: "w1", Title: title, Body: body, Tags: tags})
	if err != nil {
		t.Fatalf("Write(%q): %v", title, err)
	}
	return id
}

// AC: memory tools operational on SQLite FTS5; ranked search; isolation.
func TestStoreWriteSearchReadListDelete(t *testing.T) {
	s, dir := openTest(t)
	id1 := writeT(t, s, "tnt", dir, "Budget ladder defaults", "The warn fraction defaults to 0.25 and escalate compacts.", []string{"budget", "compact"})
	id2 := writeT(t, s, "tnt", dir, "Ollama window", "num_ctx drives the served context window.", []string{"ollama"})

	// Ranked search: exact-phrase match ranks first.
	es, err := s.Search(context.Background(), "tnt", dir, "budget ladder defaults", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(es) != 1 || es[0].ID != id1 {
		t.Fatalf("Search = %+v, want entry %d", es, id1)
	}
	if es[0].Snippet == "" {
		t.Errorf("Search snippet empty, want highlight")
	}

	// Phrase-quote sanitization: FTS operator chars are literal, no error.
	if _, err := s.Search(context.Background(), "tnt", dir, `warn OR "broken`, 10); err != nil {
		t.Errorf("Search with FTS metachars: %v", err)
	}

	// Isolation: another tenant / project sees nothing.
	if _, err := s.Read(context.Background(), "tnt2", dir, id1); !errors.Is(err, ErrNotFound) {
		t.Errorf("Read cross-tenant = %v, want ErrNotFound", err)
	}
	if es, _ := s.List(context.Background(), "tnt", filepath.Join(dir, "other"), 10, 0); len(es) != 0 {
		t.Errorf("List cross-project = %+v, want empty", es)
	}

	// List newest-first.
	ls, err := s.List(context.Background(), "tnt", dir, 10, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(ls) != 2 || ls[0].ID != id2 || ls[1].ID != id1 {
		t.Errorf("List order = %+v, want [%d %d]", ls, id2, id1)
	}

	// Replace (update = delete + reinsert).
	nid, err := s.Write(context.Background(), WriteInput{TenantID: "tnt", ProjectDir: dir, Title: "Budget ladder defaults", Body: "Updated body with tokenize terms", Tags: []string{"budget"}, ID: id1})
	if err != nil {
		t.Fatalf("Write replace: %v", err)
	}
	e, err := s.Read(context.Background(), "tnt", dir, nid)
	if err != nil {
		t.Fatalf("Read replaced: %v", err)
	}
	if e.Body != "Updated body with tokenize terms" {
		t.Errorf("Replaced body = %q", e.Body)
	}

	// Delete.
	if err := s.Delete(context.Background(), "tnt", dir, nid); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Read(context.Background(), "tnt", dir, nid); !errors.Is(err, ErrNotFound) {
		t.Errorf("Read deleted = %v, want ErrNotFound", err)
	}
}

// AC: memory.db created at <project_dir>/.orchicon/memory.db.
func TestStoreDBLocation(t *testing.T) {
	_, dir := openTest(t)
	matches, _ := filepath.Glob(filepath.Join(dir, ".orchicon", "memory.db"))
	if len(matches) != 1 {
		t.Errorf("memory.db not at project .orchicon dir: %v", matches)
	}
}

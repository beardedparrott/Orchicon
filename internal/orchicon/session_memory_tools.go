package orchicon

// session_memory_tools.go registers the four durable memory tools
// (memory_write / memory_search / memory_read / memory_list) as
// session-scoped native tools backed by the agentmemory FTS5 store.
// They are name-gated and answered by the loop's nativeToolName path —
// never routed to the MCP registry. When no store is configured the
// tools respond with a clear error (never a silent no-op).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/beardedparrott/orchicon/internal/agentmemory"
)

// memoryToolDefs returns the four durable memory tool definitions.
func memoryToolDefs() []ToolDef {
	return []ToolDef{
		{
			Name:        "memory_write",
			Description: "Persist a durable cross-session memory entry (title + body + optional tags) in this project's memory store. Pass id to replace an existing entry (returns the new id). These entries survive per-execution isolation and are searchable by later sessions.",
			ParamsJSON:  `{"type":"object","properties":{"title":{"type":"string","description":"Short title."},"body":{"type":"string","description":"Note content (one fact / root cause / decision)."},"tags":{"type":"array","items":{"type":"string"},"description":"Optional tags."},"id":{"type":"integer","description":"Existing entry id to replace."}},"required":["title","body"]}`,
		},
		{
			Name:        "memory_search",
			Description: "Ranked full-text search over durable memory entries. Query is matched as a literal phrase. Returns id, title, snippet, tags, created_at.",
			ParamsJSON:  `{"type":"object","properties":{"query":{"type":"string","description":"Plain-text search phrase."},"limit":{"type":"integer","description":"Max results (default 10, max 50)."}},"required":["query"]}`,
		},
		{
			Name:        "memory_read",
			ParamsJSON:  `{"type":"object","properties":{"id":{"type":"integer"}},"required":["id"]}`,
			Description: "Read one durable memory entry by id (full body).",
		},
		{
			Name:        "memory_list",
			ParamsJSON:  `{"type":"object","properties":{"limit":{"type":"integer"},"offset":{"type":"integer"}}}`,
			Description: "List recent durable memory entries (newest first): id, title, tags, created_at.",
		},
	}
}

// isMemoryTool reports whether a name is one of the four durable memory
// tools.
func isMemoryTool(name string) bool {
	switch name {
	case "memory_write", "memory_search", "memory_read", "memory_list":
		return true
	}
	return false
}

// execMemoryTool executes one memory tool against the session's store.
// A nil store returns a clear error, never a silent success.
func (s *Session) execMemoryTool(ctx context.Context, name, argsJSON string) (string, error) {
	if s.memStore == nil {
		return "", fmt.Errorf("agent memory is unavailable in this session (no memory store configured)")
	}
	if !s.mp.Enabled {
		return "", fmt.Errorf("agent memory is disabled for this execution")
	}
	filter := agentmemory.WriteInput{TenantID: s.identity.TenantID, ProjectDir: s.projectDir}
	switch name {
	case "memory_write":
		var a struct {
			Title string   `json:"title"`
			Body  string   `json:"body"`
			Tags  []string `json:"tags"`
			ID    int64    `json:"id"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
			return "", fmt.Errorf("memory_write: bad args: %v", err)
		}
		id, err := s.memStore.Write(ctx, agentmemory.WriteInput{
			TenantID:    filter.TenantID,
			ProjectDir:  filter.ProjectDir,
			ExecutionID: s.identity.ExecutionID,
			WorkerID:    s.identity.WorkerID,
			Title:       a.Title,
			Body:        a.Body,
			Tags:        a.Tags,
			ID:          a.ID,
		})
		if err != nil {
			return "", fmt.Errorf("memory_write: %v", err)
		}
		return fmt.Sprintf(`{"id":%d}`, id), nil
	case "memory_search":
		var a struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
			return "", fmt.Errorf("memory_search: bad args: %v", err)
		}
		entries, err := s.memStore.Search(ctx, filter.TenantID, filter.ProjectDir, a.Query, a.Limit)
		if err != nil {
			return "", fmt.Errorf("memory_search: %v", err)
		}
		b, _ := json.Marshal(entries)
		return string(b), nil
	case "memory_read":
		var a struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
			return "", fmt.Errorf("memory_read: bad args: %v", err)
		}
		e, err := s.memStore.Read(ctx, filter.TenantID, filter.ProjectDir, a.ID)
		if err != nil {
			return "", fmt.Errorf("memory_read: %v", err)
		}
		b, _ := json.Marshal(e)
		return string(b), nil
	case "memory_list":
		var a struct {
			Limit  int `json:"limit"`
			Offset int `json:"offset"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
			return "", fmt.Errorf("memory_list: bad args: %v", err)
		}
		entries, err := s.memStore.List(ctx, filter.TenantID, filter.ProjectDir, a.Limit, a.Offset)
		if err != nil {
			return "", fmt.Errorf("memory_list: %v", err)
		}
		b, _ := json.Marshal(entries)
		return string(b), nil
	}
	return "", fmt.Errorf("unknown memory tool %q", name)
}

// memoryDigest renders the capped titles+tags digest of the most recent
// memory entries for the post-breakpoint mutable zone (D3). Returns ""
// when memory is disabled/unavailable or there are no entries. Capped at
// DigestEntries entries and ~1 KiB — never leaks bodies.
func (s *Session) memoryDigest() string {
	if s.memStore == nil || !s.mp.Enabled {
		return ""
	}
	entries, err := s.memStore.List(context.Background(), s.identity.TenantID, s.projectDir, s.mp.DigestEntries, 0)
	if err != nil || len(entries) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("### Durable memory (project)\n")
	for _, e := range entries {
		tags := ""
		if len(e.Tags) > 0 {
			tags = " [" + strings.Join(e.Tags, " ") + "]"
		}
		fmt.Fprintf(&sb, "- %s%s\n", e.Title, tags)
	}
	out := sb.String()
	if len(out) > 1024 {
		out = out[:1024]
	}
	return out
}

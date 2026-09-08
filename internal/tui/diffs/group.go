package diffs

import (
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// GroupByFile flattens a chronologically-ordered edit list into per-file
// groups. Adds/Dels are tallies of "+"/"-" lines across the file's LATEST
// unified_diff; lastTool/lastAt come from the newest edit. `Kind` is the
// newest edit's kind (create/modify/delete). Mirrors sideBySide.ts
// groupByFile.
func GroupByFile(edits []*apiv1.FileEdit) []FileGroup {
	order := []string{}
	byPath := map[string]*FileGroup{}
	for _, e := range edits {
		if e == nil {
			continue
		}
		path := e.GetPath()
		g, ok := byPath[path]
		if !ok {
			g = &FileGroup{
				Path:     path,
				Kind:     e.GetKind(),
				Edits:    []*apiv1.FileEdit{},
				LastTool: e.GetTool(),
			}
			byPath[path] = g
			order = append(order, path)
		}
		g.Edits = append(g.Edits, e)
		g.Kind = e.GetKind()
		g.LastTool = e.GetTool()
		if t := e.GetCreatedAt().GetSeconds(); t > g.LastAt {
			g.LastAt = t
		}
	}
	// Compute adds/dels from the last edit's unified diff.
	for _, path := range order {
		g := byPath[path]
		last := g.Edits[len(g.Edits)-1]
		if last.GetUnifiedDiff() == "" {
			continue
		}
		rows := ParseUnifiedDiff(last.GetUnifiedDiff())
		for _, r := range rows {
			if r.Kind == KindAdd {
				g.Adds++
			} else if r.Kind == KindDel {
				g.Dels++
			}
		}
	}
	out := make([]FileGroup, 0, len(order))
	for _, path := range order {
		out = append(out, *byPath[path])
	}
	return out
}

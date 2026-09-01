package orchicon

import (
	"encoding/json"
	"io"
	"strings"
	"sync"
)

// catalog accessors (D8). The embedded JSON is parsed once.

type catalogFile struct {
	Meta struct {
		Description string `json:"description"`
		Authored    string `json:"authored"`
	} `json:"_meta"`
}

var (
	catalogOnce   sync.Once
	catalogModels map[string]ModelInfo // "provider/id" → model
)

func loadCatalog() {
	catalogOnce.Do(func() {
		catalogModels = map[string]ModelInfo{}
		f, err := catalogFS.Open("catalog.json")
		if err != nil {
			return
		}
		defer f.Close()
		// File has _meta + provider/id keys; decode generically preserving order
		// is unnecessary — map decode is fine.
		all := map[string]json.RawMessage{}
		b, _ := io.ReadAll(f)
		if err := json.Unmarshal(b, &all); err != nil {
			return
		}
		var meta catalogFile
		_ = json.Unmarshal(all["_meta"], &meta)
		for key, raw := range all {
			if key == "_meta" {
				continue
			}
			var m ModelInfo
			if err := json.Unmarshal(raw, &m); err != nil {
				continue
			}
			// id defaults to the key's model part
			if m.ID == "" {
				if i := strings.Index(key, "/"); i >= 0 {
					m.ID = key[i+1:]
				} else {
					m.ID = key
				}
			}
			m.Provenance = "catalog"
			// json null pricing decodes to nil pointer — good (billing applies).
			catalogModels[key] = m
		}
	})
}

// catalogRefKey normalizes a model_ref (provider/id) or bare id + provider.
func catalogRefKey(provider, id string) string { return provider + "/" + id }

// GetModel is the single catalog lookup by provider/id ref. The
// context-management task consumes it for compaction triggers.
func GetModel(ref string) (ModelInfo, bool) {
	loadCatalog()
	m, ok := catalogModels[ref]
	return m, ok
}

// GetModelForProvider looks up a bare model id under one provider,
// including alias matches.
func GetModelForProvider(provider, id string) (ModelInfo, bool) {
	loadCatalog()
	if m, ok := catalogModels[catalogRefKey(provider, id)]; ok {
		return m, true
	}
	for key, m := range catalogModels {
		if !strings.HasPrefix(key, provider+"/") {
			continue
		}
		for _, a := range m.Aliases {
			if a == id {
				return m, true
			}
		}
	}
	return ModelInfo{}, false
}

// catalogListByProvider returns visible catalog models for one provider.
func catalogListByProvider(provider string) []ModelInfo {
	loadCatalog()
	prefix := provider + "/"
	var out []ModelInfo
	for key, m := range catalogModels {
		if strings.HasPrefix(key, prefix) && m.Visible {
			mc := m
			out = append(out, mc)
		}
	}
	return out
}

// CatalogProviders returns catalog model counts per provider (picker use).
func CatalogProviders() map[string]int {
	loadCatalog()
	counts := map[string]int{}
	for key, m := range catalogModels {
		if !m.Visible {
			continue
		}
		if i := strings.Index(key, "/"); i > 0 {
			counts[key[:i]]++
		}
	}
	return counts
}

// BillingDisclaimer is the explicit disclaimer for zero-priced models.
const BillingDisclaimer = "no catalog pricing for this model — billing applies; displayed cost is zero, not free"

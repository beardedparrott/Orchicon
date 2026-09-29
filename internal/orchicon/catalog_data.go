package orchicon

import (
	"encoding/json"
	"io"
	"sort"
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

// catalogRefKey builds the catalog's "provider/id" key from a provider and a
// bare model id. The catalog is keyed provider/id — this is the catalog's own
// key format, NOT the 3-segment model_ref grammar (adapter/provider/model,
// ADR-0003).
func catalogRefKey(provider, id string) string { return provider + "/" + id }

// GetModel looks a model up by its "provider/id" CATALOG KEY. The catalog is
// keyed provider/id, so a canonical 3-segment model_ref
// (adapter/provider/model, ADR-0003) will NOT match — strip the adapter segment
// first (see adapter.SplitForServe).
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

// CatalogModelsForProvider returns the vendored catalog's VISIBLE models for
// one provider — the OFFLINE model source for a catalog-covered provider such
// as anthropic, where the live probe may be unreachable (no network, no token)
// or the plane may not have the provider's CLI installed at all.
//
// This is the listing half of the ONE catalog-backed model/cost lookup the
// pickers and the usage-pricing resolver share; the lookup half is this
// package's GetModelForProvider (alias-aware, same authored data). It NEVER
// synthesizes: only authored catalog entries are returned, so a provider the
// catalog does not cover yields nil and every caller keeps its existing
// live/probe behaviour unchanged.
//
// Sorted by id so a picker list is stable across calls (the underlying store
// is a map, whose iteration order is deliberately random).
func CatalogModelsForProvider(provider string) []ModelInfo {
	out := catalogListByProvider(provider)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
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

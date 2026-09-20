package orchicon

import "embed"

// catalog.go is the vendored model catalog (D8): a mini models.dev, keyed
// by provider/id, with context window, max output, tool support, reasoning
// efforts, and pricing per token class. go:embed — works offline.

//go:embed catalog.json
var catalogFS embed.FS

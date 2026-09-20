package mcpsettings

// CatalogEnvVar is one default env var (or header) entry in the curated
// registry: name, optional default value, secret flag, description.
type CatalogEnvVar struct {
	Name        string `json:"name"`
	Default     string `json:"default,omitempty"`
	Secret      bool   `json:"secret,omitempty"`
	Description string `json:"description,omitempty"`
}

// CatalogEntry is one entry in the built-in curated registry (code-first
// static data — versioned with the repo, no DB seeding).
type CatalogEntry struct {
	Slug             string          `json:"slug"`
	DisplayName      string          `json:"display_name"`
	Description      string          `json:"description"`
	Transport        string          `json:"transport"`
	InstallMechanism string          `json:"install_mechanism"` // npx | uvx | docker | remote_url
	DefaultCommand   string          `json:"default_command"`
	DefaultArgs      []string        `json:"default_args"`
	DefaultEnv       []CatalogEnvVar `json:"default_env"`
	DefaultURL       string          `json:"default_url,omitempty"`
	DefaultHeaders   []CatalogEnvVar `json:"default_headers"`
	DocsURL          string          `json:"docs_url"`
	RequiredEnv      []string        `json:"required_env"`
}

// Mechanisms.
const (
	MechanismNpx       = "npx"
	MechanismUvx       = "uvx"
	MechanismDocker    = "docker"
	MechanismRemoteURL = "remote_url"
)

func env(name, def, desc string, secret bool) CatalogEnvVar {
	return CatalogEnvVar{Name: name, Default: def, Description: desc, Secret: secret}
}

// catalog is the curated registry (16 entries, matching the work item's
// "and similar"). One-click add prefills the create form; install specs
// drive auto-install (explicit-only, dry-run capable).
var catalog = []CatalogEntry{
	{
		Slug: "filesystem", DisplayName: "Filesystem",
		Description: "Read/write access to local files and directories.",
		Transport:   TransportStdio, InstallMechanism: MechanismNpx,
		DefaultCommand: "npx", DefaultArgs: []string{"-y", "@modelcontextprotocol/server-filesystem"},
		DocsURL: "https://github.com/modelcontextprotocol/servers/tree/main/src/filesystem",
	},
	{
		Slug: "github", DisplayName: "GitHub",
		Description: "GitHub API access: issues, PRs, repos, search.",
		Transport:   TransportStdio, InstallMechanism: MechanismNpx,
		DefaultCommand: "npx", DefaultArgs: []string{"-y", "@modelcontextprotocol/server-github"},
		DefaultEnv: []CatalogEnvVar{
			env("GITHUB_PERSONAL_ACCESS_TOKEN", "", "GitHub personal access token (classic, repo scope)", true),
		},
		DocsURL:     "https://github.com/github/github-mcp-server",
		RequiredEnv: []string{"GITHUB_PERSONAL_ACCESS_TOKEN"},
	},
	{
		Slug: "gitlab", DisplayName: "GitLab",
		Description: "GitLab API access: projects, MRs, issues.",
		Transport:   TransportStdio, InstallMechanism: MechanismNpx,
		DefaultCommand: "npx", DefaultArgs: []string{"-y", "@modelcontextprotocol/server-gitlab"},
		DefaultEnv: []CatalogEnvVar{
			env("GITLAB_PERSONAL_ACCESS_TOKEN", "", "GitLab personal access token", true),
			env("GITLAB_API_URL", "https://gitlab.com", "GitLab instance base URL", false),
		},
		DocsURL:     "https://github.com/modelcontextprotocol/servers/tree/main/src/gitlab",
		RequiredEnv: []string{"GITLAB_PERSONAL_ACCESS_TOKEN"},
	},
	{
		Slug: "postgres", DisplayName: "Postgres",
		Description: "Query and inspect Postgres databases.",
		Transport:   TransportStdio, InstallMechanism: MechanismNpx,
		DefaultCommand: "npx", DefaultArgs: []string{"-y", "@modelcontextprotocol/server-postgres"},
		DefaultEnv: []CatalogEnvVar{
			env("DATABASE_URL", "postgresql://localhost:5432/mydb", "Postgres connection string", false),
		},
		DocsURL:     "https://github.com/modelcontextprotocol/servers/tree/main/src/postgres",
		RequiredEnv: []string{"DATABASE_URL"},
	},
	{
		Slug: "sqlite", DisplayName: "SQLite",
		Description: "Inspect and query SQLite database files.",
		Transport:   TransportStdio, InstallMechanism: MechanismNpx,
		DefaultCommand: "npx", DefaultArgs: []string{"-y", "mcp-server-sqlite"},
		DocsURL: "https://github.com/modelcontextprotocol/servers/tree/main/src/sqlite",
	},
	{
		Slug: "fetch", DisplayName: "Fetch",
		Description: "Fetch web pages and convert them to readable markdown.",
		Transport:   TransportStdio, InstallMechanism: MechanismNpx,
		DefaultCommand: "npx", DefaultArgs: []string{"-y", "@modelcontextprotocol/server-fetch"},
		DocsURL: "https://github.com/modelcontextprotocol/servers/tree/main/src/fetch",
	},
	{
		Slug: "playwright", DisplayName: "Playwright",
		Description: "Browser automation: navigate, click, extract, screenshot.",
		Transport:   TransportStdio, InstallMechanism: MechanismNpx,
		DefaultCommand: "npx", DefaultArgs: []string{"-y", "@playwright/mcp@latest"},
		DocsURL: "https://github.com/microsoft/playwright-mcp",
	},
	{
		Slug: "puppeteer", DisplayName: "Puppeteer",
		Description: "Headless Chrome automation via the Puppeteer MCP server.",
		Transport:   TransportStdio, InstallMechanism: MechanismNpx,
		DefaultCommand: "npx", DefaultArgs: []string{"-y", "@modelcontextprotocol/server-puppeteer"},
		DocsURL: "https://github.com/modelcontextprotocol/servers/tree/main/src/puppeteer",
	},
	{
		Slug: "sentry", DisplayName: "Sentry",
		Description: "Sentry issues, releases, and project metrics.",
		Transport:   TransportStdio, InstallMechanism: MechanismUvx,
		DefaultCommand: "uvx", DefaultArgs: []string{"mcp-server-sentry"},
		DefaultEnv: []CatalogEnvVar{
			env("SENTRY_AUTH_TOKEN", "", "Sentry auth token", true),
			env("SENTRY_ORG_SLUG", "", "Sentry organization slug", false),
		},
		DocsURL:     "https://github.com/getsentry/sentry-mcp",
		RequiredEnv: []string{"SENTRY_AUTH_TOKEN"},
	},
	{
		Slug: "slack", DisplayName: "Slack",
		Description: "Slack workspace access: channels, messages, history.",
		Transport:   TransportStdio, InstallMechanism: MechanismUvx,
		DefaultCommand: "uvx", DefaultArgs: []string{"mcp-server-slack"},
		DefaultEnv: []CatalogEnvVar{
			env("SLACK_BOT_TOKEN", "", "Slack bot token (xoxb-...)", true),
		},
		DocsURL:     "https://github.com/modelcontextprotocol/servers/tree/main/src/slack",
		RequiredEnv: []string{"SLACK_BOT_TOKEN"},
	},
	{
		Slug: "memory", DisplayName: "Memory (knowledge graph)",
		Description: "Persistent knowledge-graph memory across sessions.",
		Transport:   TransportStdio, InstallMechanism: MechanismNpx,
		DefaultCommand: "npx", DefaultArgs: []string{"-y", "@modelcontextprotocol/server-memory"},
		DocsURL: "https://github.com/modelcontextprotocol/servers/tree/main/src/memory",
	},
	{
		Slug: "context7", DisplayName: "Context7",
		Description: "Up-to-date library documentation for any framework (remote).",
		Transport:   TransportStreamable, InstallMechanism: MechanismRemoteURL,
		DefaultURL: "https://mcp.context7.com/mcp",
		DocsURL:    "https://context7.com",
	},
	{
		Slug: "sequential-thinking", DisplayName: "Sequential Thinking",
		Description: "Structured multi-step reasoning via sequential thought.",
		Transport:   TransportStdio, InstallMechanism: MechanismNpx,
		DefaultCommand: "npx", DefaultArgs: []string{"-y", "@modelcontextprotocol/server-sequential-thinking"},
		DocsURL: "https://github.com/modelcontextprotocol/servers/tree/main/src/sequentialthinking",
	},
	{
		Slug: "time", DisplayName: "Time",
		Description: "Current date/time in any timezone; timezone conversion.",
		Transport:   TransportStdio, InstallMechanism: MechanismNpx,
		DefaultCommand: "npx", DefaultArgs: []string{"-y", "@modelcontextprotocol/server-time"},
		DocsURL: "https://github.com/modelcontextprotocol/servers/tree/main/src/time",
	},
	{
		Slug: "everart", DisplayName: "Everart",
		Description: "Generate images with Everart models (API key required).",
		Transport:   TransportStdio, InstallMechanism: MechanismNpx,
		DefaultCommand: "npx", DefaultArgs: []string{"-y", "@everart/mcp-server"},
		DocsURL: "https://github.com/everartai/everart-mcp-server",
	},
	{
		Slug: "git", DisplayName: "Git (MCP)",
		Description: "Git operations over MCP: status, diff, commit, log.",
		Transport:   TransportStdio, InstallMechanism: MechanismNpx,
		DefaultCommand: "npx", DefaultArgs: []string{"-y", "mcp-server-git"},
		DocsURL: "https://github.com/modelcontextprotocol/servers/tree/main/src/git",
	},
}

// ListCatalog returns the curated registry (static; no DB reads).
func ListCatalog() []CatalogEntry {
	out := make([]CatalogEntry, len(catalog))
	copy(out, catalog)
	return out
}

// catalogBySlug resolves an entry by its stable slug.
func catalogBySlug(slug string) (CatalogEntry, bool) {
	for _, c := range catalog {
		if c.Slug == slug {
			return c, true
		}
	}
	return CatalogEntry{}, false
}

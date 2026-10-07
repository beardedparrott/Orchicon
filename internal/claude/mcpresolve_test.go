package claude

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/mcpclient"
)

// fakeMCPConfigSource is a ScopeResolver backed by literals — the seam every
// adapter consumes, so a test can drive the full resolution without storage.
// It returns `servers` for EVERY scope (the union shape); `project`/`worker`
// are the ids it reports as selected, kept so the existing assertions read
// the same way.
type fakeMCPConfigSource struct {
	servers []mcpclient.ServerSpec
	worker  []string
	project []string
	listErr error
	selErr  error
}

func (f *fakeMCPConfigSource) ResolveScope(_ context.Context, _ mcpclient.ScopeRef) (mcpclient.Resolution, error) {
	if f.listErr != nil {
		return mcpclient.Resolution{}, f.listErr
	}
	if f.selErr != nil {
		return mcpclient.Resolution{}, f.selErr
	}
	byID := map[string]mcpclient.ServerSpec{}
	for _, sp := range f.servers {
		byID[sp.ID] = sp
	}
	sel := f.project
	if len(sel) == 0 {
		sel = f.worker
	}
	var res mcpclient.Resolution
	for _, id := range sel {
		res.SelectedIDs = append(res.SelectedIDs, id)
		sp, ok := byID[id]
		if !ok {
			// A selected id with no matching definition is REPORTED as
			// missing — the resolution contract the adapters surface.
			res.Missing = append(res.Missing, id)
			continue
		}
		res.Servers = append(res.Servers, mcpclient.ScopedServer{Spec: sp, EntryID: sp.ID, From: mcpclient.ScopeProject})
	}
	return res, nil
}

func builtinFor(t *testing.T) MCPServer {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(ClaudeBinEnv, "")
	installFakeClaude(t, home)
	return OrchiconMCPServer(HookBinaryPath(), "tnt_dev", nil)
}

// THE AGNOSTIC CONTRACT: the same ScopeResolver the native bridge consumes drives
// claude's set, so the two adapters cannot disagree about WHICH servers apply.
func TestResolveMCPServersUsesTheSharedSource(t *testing.T) {
	b := New(quietLogger())
	b.SetScopeResolver(&fakeMCPConfigSource{
		servers: []mcpclient.ServerSpec{
			{ID: "stdio-srv", Command: []string{"/usr/bin/thing", "--flag"}, Env: map[string]string{"K": "V"}},
			{ID: "http-srv", URL: "https://mcp.example.com", Headers: map[string]string{"Authorization": "Bearer x"}},
		},
		project: []string{"stdio-srv", "http-srv"},
	})

	got, err := b.resolveMCPServers(context.Background(), "tnt_dev", "", "proj-1", builtinFor(t))
	if err != nil {
		t.Fatalf("resolveMCPServers: %v", err)
	}
	// The built-in is FIRST and the resolved set follows — never instead.
	if len(got) != 3 {
		t.Fatalf("got %d servers, want the built-in + 2 resolved: %+v", len(got), got)
	}
	if got[0].Name != "orchicon" {
		t.Errorf("first server = %q, want the built-in orchicon", got[0].Name)
	}
	// The argv-slice Command splits into command + args.
	byName := map[string]MCPServer{}
	for _, s := range got {
		byName[s.Name] = s
	}
	if s := byName["stdio-srv"]; s.Command != "/usr/bin/thing" || len(s.Args) != 1 || s.Args[0] != "--flag" {
		t.Errorf("stdio spec rendered as %+v, want command=/usr/bin/thing args=[--flag]", s)
	}
	if s := byName["http-srv"]; s.URL != "https://mcp.example.com" || s.Headers["Authorization"] != "Bearer x" {
		t.Errorf("http spec rendered as %+v", s)
	}
}

// NO SOURCE must still yield the built-in: a plane with no MCP storage wiring (or
// no tenant servers) must keep `orchicon_*` tools, which is the state the
// operator found claude in.
func TestResolveMCPServersAlwaysRegistersTheBuiltin(t *testing.T) {
	b := New(quietLogger())
	got, err := b.resolveMCPServers(context.Background(), "tnt_dev", "", "", builtinFor(t))
	if err != nil {
		t.Fatalf("resolveMCPServers: %v", err)
	}
	if len(got) != 1 || got[0].Name != "orchicon" {
		t.Fatalf("got %+v, want exactly the built-in", got)
	}

	// An empty SELECTION is also "no operator servers", not an error.
	b2 := New(quietLogger())
	b2.SetScopeResolver(&fakeMCPConfigSource{
		servers: []mcpclient.ServerSpec{{ID: "s1", Command: []string{"/bin/s1"}}},
	})
	got2, err := b2.resolveMCPServers(context.Background(), "tnt_dev", "", "", builtinFor(t))
	if err != nil {
		t.Fatalf("resolveMCPServers (empty selection): %v", err)
	}
	if len(got2) != 1 {
		t.Fatalf("an empty selection must yield the built-in alone, got %+v", got2)
	}
}

// A SELECTED BUT UNCONFIGURED server is the native bridge's documented failure,
// and it must fail here too: a worker running without the MCP servers it was
// configured to have looks healthy and cannot do its job.
func TestResolveMCPServersFailsOnAMissingSelection(t *testing.T) {
	b := New(quietLogger())
	b.SetScopeResolver(&fakeMCPConfigSource{
		servers: []mcpclient.ServerSpec{{ID: "configured", Command: []string{"/bin/x"}}},
		project: []string{"configured", "ghost"},
	})
	_, err := b.resolveMCPServers(context.Background(), "tnt_dev", "", "proj-1", builtinFor(t))
	if err == nil {
		t.Fatal("a selected-but-unconfigured server was accepted; a worker would run without its tools")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("the error does not name the missing id: %v", err)
	}
}

// A source error must surface, not be swallowed into "no servers".
func TestResolveMCPServersSurfacesASourceError(t *testing.T) {
	b := New(quietLogger())
	b.SetScopeResolver(&fakeMCPConfigSource{listErr: errors.New("storage down")})
	if _, err := b.resolveMCPServers(context.Background(), "tnt_dev", "", "", builtinFor(t)); err == nil {
		t.Fatal("a source error was swallowed")
	}
}

// ${SECRET_NAME} refs are expanded through the injected resolver, on the SAME
// path the native bridge uses.
func TestResolveMCPServersExpandsSecrets(t *testing.T) {
	b := New(quietLogger())
	b.SetScopeResolver(&fakeMCPConfigSource{
		servers: []mcpclient.ServerSpec{{
			ID:      "github",
			Command: []string{"/bin/gh-mcp"},
			Env:     map[string]string{"GITHUB_PERSONAL_ACCESS_TOKEN": "${GITHUB_TOKEN}"},
		}},
		project: []string{"github"},
	})
	var sawRef bool
	b.SetMCPSecretResolver(func(_ context.Context, tenantID string, env, headers map[string]string) (map[string]string, map[string]string, error) {
		if tenantID != "tnt_dev" {
			t.Errorf("resolver got tenant %q, want tnt_dev", tenantID)
		}
		if strings.Contains(env["GITHUB_PERSONAL_ACCESS_TOKEN"], "${") {
			sawRef = true
		}
		return map[string]string{"GITHUB_PERSONAL_ACCESS_TOKEN": "ghp_real"}, headers, nil
	})

	got, err := b.resolveMCPServers(context.Background(), "tnt_dev", "", "proj-1", builtinFor(t))
	if err != nil {
		t.Fatalf("resolveMCPServers: %v", err)
	}
	if !sawRef {
		t.Error("the resolver never saw the ${...} reference it exists to expand")
	}
	for _, s := range got {
		if s.Name == "github" && s.Env["GITHUB_PERSONAL_ACCESS_TOKEN"] != "ghp_real" {
			t.Errorf("secret not expanded: %v", s.Env)
		}
	}
}

// A resolver failure must fail the launch, not emit a config with a literal
// ${SECRET} in it — that would hand the CLI a token-shaped string that is not a
// token, and the server would just fail to authenticate.
func TestResolveMCPServersFailsOnASecretError(t *testing.T) {
	b := New(quietLogger())
	b.SetScopeResolver(&fakeMCPConfigSource{
		servers: []mcpclient.ServerSpec{{ID: "s", Command: []string{"/bin/s"}, Env: map[string]string{"K": "${NOPE}"}}},
		project: []string{"s"},
	})
	b.SetMCPSecretResolver(func(context.Context, string, map[string]string, map[string]string) (map[string]string, map[string]string, error) {
		return nil, nil, errors.New("secret NOPE not found")
	})
	if _, err := b.resolveMCPServers(context.Background(), "tnt_dev", "", "p", builtinFor(t)); err == nil {
		t.Fatal("a secret resolution failure was ignored")
	}
}

// END TO END AT THE ARGV. This is the one that matters for the operator's
// request: a project's configured MCP server must reach the `--mcp-config`
// claude is actually launched with, alongside the built-in.
func TestAskArgvCarriesResolvedOperatorMCPs(t *testing.T) {
	builtinFor(t) // installs the fake CLI + HOME
	b := New(quietLogger())
	b.SetScopeResolver(&fakeMCPConfigSource{
		servers: []mcpclient.ServerSpec{
			{ID: "sentry", URL: "https://mcp.sentry.dev", Headers: map[string]string{"X": "1"}},
		},
		project: []string{"sentry"},
	})
	s := newAskSession(b, "conv-op", filepath.Join(t.TempDir(), "ask"))
	s.tenantID = "tnt_dev"

	var cfg string
	for i, a := range s.argv() {
		if a == "--mcp-config" && i+1 < len(s.argv()) {
			cfg = s.argv()[i+1]
		}
	}
	if cfg == "" {
		t.Fatal("the Ask argv has no --mcp-config")
	}
	for _, want := range []string{`"orchicon"`, `"sentry"`, "https://mcp.sentry.dev"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("rendered config is missing %q: %s", want, cfg)
		}
	}
}

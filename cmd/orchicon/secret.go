package main

// secret.go — `orchicon secret get <NAME>`: read a tenant secret.
//
// IT IS A PLAIN GET. No execution identity, no role binding, no special credential. It resolves the
// plane and the token exactly the way every other CLI surface does — ORCHICON_URL/ORCHICON_TOKEN, or
// a saved profile — and asks the plane for the secret's value. That means it works for any secret the
// caller is entitled to read, whether it is a dev-plane key, a provider token, or anything else.
//
// WHY IT IS NOT A SPECIAL "WORKER" DOOR. My first version authenticated with ORCHICON_PLANE_TOKEN and
// required a run id, which made fetching a DEV secret depend on a PROD role binding — the platform
// only mints that token for a worker with a role_ref. Two unrelated entitlements, coupled by
// accident. A secret is a value in the caller's tenant; reading it needs the caller's ordinary
// credential and nothing more.
//
// STDOUT CARRIES THE VALUE AND NOTHING ELSE, so `TOKEN=$(orchicon secret get DEV_PLANE_KEY)` works and
// the value can never be mistaken for a log line. Errors go to stderr.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/tui/config"
)

// secretFlags holds this subcommand's own overrides. It is deliberately NOT cmd/orch's flags type:
// that one belongs to the interactive client, and borrowing it would couple this to it.
type secretFlags struct {
	url, token, profile string
}

func runSecret(args []string, stdout, stderr *os.File) int {
	if len(args) == 0 || args[0] != "get" {
		fmt.Fprintln(stderr, "usage: orchicon secret get <NAME> [--url URL] [--token TOKEN] [--profile NAME]")
		fmt.Fprintln(stderr, "  Reads a tenant secret by name. Resolves the plane the same way `orch` does:")
		fmt.Fprintln(stderr, "  ORCHICON_URL + ORCHICON_TOKEN, or a saved profile (ORCHICON_PROFILE / --profile).")
		return 2
	}
	name := ""
	fl := &secretFlags{}
	for i := 1; i < len(args); i++ {
		switch a := args[i]; a {
		case "--url", "-u":
			if i+1 < len(args) {
				i++
				fl.url = args[i]
			}
		case "--token", "-t":
			if i+1 < len(args) {
				i++
				fl.token = args[i]
			}
		case "--profile", "-p":
			if i+1 < len(args) {
				i++
				fl.profile = args[i]
			}
		default:
			if strings.HasPrefix(a, "-") {
				fmt.Fprintf(stderr, "orchicon secret get: unknown flag %q\n", a)
				return 2
			}
			if name == "" {
				name = strings.TrimSpace(a)
			}
		}
	}
	if name == "" {
		fmt.Fprintln(stderr, "orchicon secret get: a secret NAME is required")
		return 2
	}

	baseURL, token, err := resolvePlane(fl)
	if err != nil {
		fmt.Fprintf(stderr, "orchicon secret get: %v\n", err)
		return 1
	}

	client := apiv1connect.NewSecretsServiceClient(
		&http.Client{Timeout: 30 * time.Second},
		baseURL,
		connect.WithInterceptors(bearerInterceptor(token)),
	)
	resp, err := client.GetSecretValue(context.Background(),
		connect.NewRequest(&apiv1.GetSecretValueRequest{Name: name}))
	if err != nil {
		// THE VALUE IS NEVER IN THIS PATH, so echoing the error is safe.
		fmt.Fprintf(stderr, "orchicon secret get %s: %v\n", name, err)
		return 1
	}
	if resp.Msg.Value == "" {
		fmt.Fprintf(stderr, "orchicon secret get %s: the platform returned an empty value\n", name)
		return 1
	}
	fmt.Fprintln(stdout, resp.Msg.Value)
	return 0
}

// resolvePlane gets the base URL and credential the same way the orch CLI does: env first, then the
// saved profile. ONE resolution path for the whole CLI means a worker that can run `orch` can run
// this, with the same credential and no extra setup.
func resolvePlane(fl *secretFlags) (string, string, error) {
	cfg := &config.Config{Profiles: map[string]*config.Profile{}}
	if path, perr := config.DefaultPath(); perr == nil {
		if loaded, lerr := config.Load(path); lerr == nil && loaded != nil {
			cfg = loaded
		}
	}
	requested := strings.TrimSpace(fl.profile)
	if requested == "" {
		requested = strings.TrimSpace(os.Getenv(config.EnvProfile))
	}
	profile := config.ResolveNamed(cfg, requested)
	baseURL, token := "", ""
	if profile != nil {
		baseURL, token = profile.URL, profile.Token
	}
	// Flag overrides are ephemeral and win, exactly like `orch`.
	if fl.url != "" {
		baseURL = fl.url
	}
	if fl.token != "" {
		token = fl.token
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return "", "", fmt.Errorf("no plane URL — set %s, pass --url, or connect once with `orch` to save a profile",
			config.EnvURL)
	}
	if token == "" {
		return "", "", fmt.Errorf("no credential for %s — set %s, pass --token, or connect once with `orch`",
			baseURL, config.EnvToken)
	}
	return baseURL, token, nil
}

// bearerInterceptor attaches the caller's credential to every request.
func bearerInterceptor(token string) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Authorization", "Bearer "+token)
			return next(ctx, req)
		}
	}
}

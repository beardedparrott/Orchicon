package main

// secret.go — `orchicon secret get <NAME>`: the worker's fetch-on-demand credential door.
//
// WHY A COMMAND AND NOT AN ENV VAR. A local-mode execution has no container to inject environment
// into, so a worker had no way to obtain a credential at all. Putting the value in the environment
// would make it readable in /proc/<pid>/environ by the same user, visible to any child process and to
// `env`. This door materialises nothing: the value is fetched when needed, over the plane's API,
// authorised against the calling run, and every read is audited.
//
// THE ENVIRONMENT IS THE CALLER'S PROOF OF IDENTITY. Every execution already carries
// ORCHICON_PLANE_URL, ORCHICON_PLANE_TOKEN and ORCHICON_MCP_WORKFLOW_RUN_ID (see
// runtime.mintPlaneCredential). The run id in the request must match the caller's own token subject —
// the SERVER enforces that, so a worker cannot ask for another run's secrets by editing a flag.
//
// STDOUT CARRIES THE VALUE AND NOTHING ELSE, so `TOKEN=$(orchicon secret get DEV_PLANE_TOKEN)` works
// and so the value can never be mistaken for a log line. Errors go to stderr.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"connectrpc.com/connect"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
)

const (
	secretPlaneURLEnv   = "ORCHICON_PLANE_URL"
	secretPlaneTokenEnv = "ORCHICON_PLANE_TOKEN"
	secretRunIDEnv      = "ORCHICON_MCP_WORKFLOW_RUN_ID"
)

func runSecret(args []string, stdout, stderr *os.File) int {
	if len(args) == 0 || args[0] != "get" {
		fmt.Fprintln(stderr, "usage: orchicon secret get <NAME>")
		fmt.Fprintln(stderr, "  Reads a tenant secret granted to this execution. Requires ORCHICON_PLANE_URL,")
		fmt.Fprintln(stderr, "  ORCHICON_PLANE_TOKEN and ORCHICON_MCP_WORKFLOW_RUN_ID, which every execution has.")
		return 2
	}
	if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
		fmt.Fprintln(stderr, "orchicon secret get: a secret NAME is required")
		return 2
	}
	name := strings.TrimSpace(args[1])

	baseURL := strings.TrimSpace(os.Getenv(secretPlaneURLEnv))
	token := strings.TrimSpace(os.Getenv(secretPlaneTokenEnv))
	runID := strings.TrimSpace(os.Getenv(secretRunIDEnv))
	// ONE MISSING VARIABLE IS A STATEMENT ABOUT THE CALLER, not a generic failure: this command only
	// works from inside an execution, and saying so is more useful than "unauthorised".
	var missing []string
	if baseURL == "" {
		missing = append(missing, secretPlaneURLEnv)
	}
	if token == "" {
		missing = append(missing, secretPlaneTokenEnv)
	}
	if runID == "" {
		missing = append(missing, secretRunIDEnv)
	}
	if len(missing) > 0 {
		fmt.Fprintf(stderr, "orchicon secret get: not running inside a worker execution (missing %s)\n",
			strings.Join(missing, ", "))
		return 1
	}

	cfg := connect.WithInterceptors(bearerInterceptor(token))
	client := apiv1connect.NewSecretsServiceClient(&http.Client{}, baseURL, cfg)
	resp, err := client.GetExecutionSecret(context.Background(),
		connect.NewRequest(&apiv1.GetExecutionSecretRequest{
			WorkflowRunId: runID,
			Name:          name,
		}))
	if err != nil {
		// THE VALUE IS NEVER IN THIS PATH, so echoing the error is safe. The server's message is
		// deliberately opaque ("not available to this run") to prevent name enumeration.
		fmt.Fprintf(stderr, "orchicon secret get %s: %v\n", name, err)
		return 1
	}
	if resp.Msg.Value == "" {
		fmt.Fprintf(stderr, "orchicon secret get %s: the platform returned an empty value\n", name)
		return 1
	}
	// STDOUT IS THE VALUE, bare, with no newline decoration beyond one trailing newline for shells.
	fmt.Fprintln(stdout, resp.Msg.Value)
	return 0
}

// bearerInterceptor attaches the execution's minted plane credential to every request.
func bearerInterceptor(token string) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if token == "" {
				return nil, errors.New("no plane token in this execution")
			}
			req.Header().Set("Authorization", "Bearer "+token)
			return next(ctx, req)
		}
	}
}

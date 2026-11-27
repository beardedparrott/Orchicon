package antigravity

// procspec.go — the EXACT `agy acp` invocation, derived from the tenant's Antigravity
// settings.
//
// WHY THIS IS A BUILDER AND NOT A FEW APPENDS AT THE SPAWN SITE. Three separate
// requirements meet in this one command line, and each is a way to leak a credential or
// silently ignore an operator choice if it is spelled inline at the call site:
//
//   1. The ACP server must be TOLD the auth method (authmethod.MethodID), because agy
//      refuses `session/new` with -32000 "Authentication required" until the client has
//      called `authenticate {methodId}`. The method is an operator setting, not a constant.
//   2. The Gemini API key is passed ONLY as the child's GEMINI_API_KEY environment
//      variable. Never on argv: argv is visible in `ps`, in the container engine's
//      command line, and in every process listing an operator or crash dump may capture.
//      For every other method the variable is STRIPPED from the inherited environment, so
//      an API key sitting in the plane's environment cannot silently take over an
//      oauth-personal session.
//   3. Telemetry is off unless the tenant explicitly opted in, so the flag is derived from
//      the setting rather than assumed.
//
// The ARGV itself is rendered by acp.LaunchOptions, so the flags are spelled in exactly
// one place; this file only decides WHICH options the settings imply.

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/beardedparrott/orchicon/internal/antigravity/acp"
	"github.com/beardedparrott/orchicon/internal/antigravity/authmethod"
)

// SpawnSettings is the slice of tenant settings that shapes the child invocation.
type SpawnSettings struct {
	// AuthMethod is one of authmethod.Methods(); "" means the default.
	AuthMethod string
	// ShareTelemetry true OMITS --disable-telemetry (the tenant opted in).
	ShareTelemetry bool
	// AskMode is the Ask MODE this child is being launched for ("" for a worker execution, which has no
	// mode). It only affects the sessionSettings half below; the worker path never sets it, so a worker's
	// argv stays byte-identical to what it was before Ask existed.
	AskMode string
	// SessionSettings is the raw `--settings` JSON document (session-only, never saved). The Ask path sets
	// it from the mode's denied tools (modegate.go) so a refused tool cannot even reach a permission
	// request. It is applied verbatim: agy rejects an unknown key and the operator's own settings file is
	// never touched.
	SessionSettings string
}

// SpawnSpec is a fully-resolved `agy acp` invocation: the argv, the complete child
// environment, and the method id to send in the ACP `authenticate` request.
type SpawnSpec struct {
	Argv []string
	Env  []string
	// AuthMethod is the NORMALIZED method (never ""), ready for authenticate{methodId}.
	AuthMethod string
}

// BuildSpawnSpec builds the invocation for one execution.
//
// bin is the resolved agy path ("" = "agy" on PATH). baseEnv is the environment the child
// would otherwise inherit (os.Environ(), or the container's env list). apiKey is the
// tenant GEMINI_API_KEY secret, decrypted by the plane — it is required only by the
// gemini-api-key method.
func BuildSpawnSpec(bin string, s SpawnSettings, apiKey string, baseEnv []string) (SpawnSpec, error) {
	if err := authmethod.Validate(s.AuthMethod); err != nil {
		return SpawnSpec{}, err
	}
	method := authmethod.MethodID(s.AuthMethod)
	if bin == "" {
		bin = "agy"
	}

	// THE ONE PLACE THE FLAGS ARE SPELLED: acp owns the argv shape, this owns the policy.
	argv := acp.LaunchOptions{
		Binary:           bin,
		DisableTelemetry: !s.ShareTelemetry,
		// THE SESSION-ONLY SETTINGS (Ask modes only; "" on the worker path, which adds no argv element).
		Settings: s.SessionSettings,
	}.Argv()

	// A `--settings` DOCUMENT THAT DISABLES THE PERMISSION SYSTEM IS A BYPASS, and assertNoBypass is about
	// argv spellings rather than JSON. The deny list this adapter emits only ever SUBTRACTS capability, so
	// the guard is that the document must not name an allow/auto-approve key at all.
	if err := assertSettingsNotBypass(s.SessionSettings); err != nil {
		return SpawnSpec{}, err
	}

	// Strip any inherited key FIRST, then add it back only for the method that uses one.
	// Order matters: appending without stripping would leave two values for one name, and
	// which one the child sees is up to the platform.
	env := withoutEnv(baseEnv, authmethod.APIKeySecretName)
	if authmethod.NeedsAPIKey(method) {
		key := strings.TrimSpace(apiKey)
		if key == "" {
			return SpawnSpec{}, fmt.Errorf("%s auth method selected but no %s secret is set "+
				"(create a tenant secret named %s)",
				method, authmethod.APIKeySecretName, authmethod.APIKeySecretName)
		}
		env = append(env, authmethod.APIKeySecretName+"="+key)
	}
	return SpawnSpec{Argv: argv, Env: env, AuthMethod: method}, nil
}

// ProcSpec maps this onto the client's spawn request. The caller fills ExecID, Cwd,
// ProjectDir and (for container mode) WorkflowRunID.
func (s SpawnSpec) ProcSpec() acp.ProcSpec {
	return acp.ProcSpec{Argv: s.Argv, Env: s.Env}
}

// String renders the spec WITHOUT any environment VALUE — names only. The child's
// environment contains the API key, so a spec that prints its env is a credential leak in
// every log line, error message and `%v` a caller writes.
func (s SpawnSpec) String() string {
	return fmt.Sprintf("agy argv=%s env=[%s] auth=%s",
		strings.Join(s.Argv, " "), strings.Join(s.EnvNames(), " "), s.AuthMethod)
}

// LogValue is the structured-logging form: same redaction as String.
func (s SpawnSpec) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("argv", strings.Join(s.Argv, " ")),
		slog.String("env_keys", strings.Join(s.EnvNames(), " ")),
		slog.String("auth_method", s.AuthMethod),
	)
}

// EnvNames lists the variable NAMES in the child environment, sorted, with no values.
func (s SpawnSpec) EnvNames() []string {
	names := make([]string, 0, len(s.Env))
	for _, kv := range s.Env {
		name, _, _ := strings.Cut(kv, "=")
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// withoutEnv returns env with every entry for name removed, so an inherited value can never
// shadow — or stand in for — the one this spec decided on.
func withoutEnv(env []string, name string) []string {
	prefix := name + "="
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

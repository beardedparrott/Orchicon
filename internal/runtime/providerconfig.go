package runtime

// ProviderConfig is one provider as a runtime container's serve config needs it: the
// id, the npm package that speaks its wire protocol, and the BASE URL THAT THIS
// CONSUMER CAN ACTUALLY DIAL.
//
// IT LIVES IN runtime, NOT opencode, FOR A DEPENDENCY REASON: opencode imports runtime,
// so runtime cannot import opencode; and providers imports opencode, so opencode cannot
// import providers. This is the only package all three can share — and the container-serve
// contract is runtime's anyway (CreateRequest.ServeConfig), so a block that travels inside
// that config belongs here.
//
// BaseURL is expected to be TRANSPOSED for the container already — see
// providers.TransposeForContainer — so this type carries no locality of its own. That split
// is deliberate: transposition is a property of WHERE the client runs, and it is applied by
// the caller that knows, never guessed here.
type ProviderConfig struct {
	// ID is the opencode provider id (the middle segment of a model_ref:
	// opencode/<id>/<model>).
	ID string
	// NPM is the package implementing the provider's protocol, e.g.
	// "@ai-sdk/openai-compatible" for an OpenAI-compatible local server.
	NPM string
	// BaseURL is the endpoint THIS consumer dials (already transposed).
	BaseURL string
}

package runtime

// ProviderConfig is one provider as a runtime container's serve config needs it: the id, the npm
// package that speaks its wire protocol, and the BASE URL THAT THIS CONSUMER CAN ACTUALLY DIAL.
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

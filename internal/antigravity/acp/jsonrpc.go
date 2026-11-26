package acp

import "encoding/json"

// JSON-RPC 2.0 wire types.
//
// IDS ARE json.RawMessage EVERYWHERE, and that is not laziness. agy sends its own JSON-RPC ids
// as STRINGS ("id":"1" on session/request_permission) while the ids this client allocates are
// numbers; echoing a decoded value back would silently change the type and either break the
// correlation or confuse agy. Raw bytes are echoed verbatim.

// rpcMessage is ANY inbound document: request, notification or response.
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// rpcRequest is an OUTBOUND request (always carries an id).
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// rpcNotification is an OUTBOUND notification (never carries an id).
type rpcNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// rpcResult is an OUTBOUND answer to an agent request.
type rpcResult struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
}

// rpcFailure is an OUTBOUND error answer to an agent request.
type rpcFailure struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   *RPCError       `json:"error"`
}

// hasID reports whether an inbound document carries a usable id.
func hasID(id json.RawMessage) bool {
	return len(id) > 0 && string(id) != "null"
}

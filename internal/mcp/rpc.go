package mcp

import (
	"encoding/json"
)

// rpcRequest is the wire shape of an incoming JSON-RPC 2.0 call.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// rpcResponse is the wire shape of a JSON-RPC 2.0 reply. Exactly one
// of Result and Error is set.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func ok(id json.RawMessage, payload any) *rpcResponse {
	raw, _ := json.Marshal(payload)
	return &rpcResponse{JSONRPC: "2.0", ID: id, Result: raw}
}

func rpcErr(id json.RawMessage, code int, msg string, data any) *rpcResponse {
	var raw json.RawMessage
	if data != nil {
		raw, _ = json.Marshal(data)
	}
	return &rpcResponse{
		JSONRPC: "2.0", ID: id,
		Error: &rpcError{Code: code, Message: msg, Data: raw},
	}
}

func parseError(id json.RawMessage, detail string) *rpcResponse {
	return rpcErr(id, -32700, "parse error: "+detail, nil)
}

func methodNotFound(id json.RawMessage, method string) *rpcResponse {
	return rpcErr(id, -32601, "method not found: "+method, nil)
}

func invalidParams(id json.RawMessage, detail string) *rpcResponse {
	return rpcErr(id, -32602, "invalid params: "+detail, nil)
}

// tabSummary is the result row for tabs/list.
type tabSummary struct {
	ID       uint32 `json:"id"`
	Name     string `json:"name,omitempty"`
	Title    string `json:"title"`
	Cols     uint16 `json:"cols"`
	Rows     uint16 `json:"rows"`
	WindowID uint32 `json:"window_id"`
	Focused  bool   `json:"focused"`
}

// screenResult is the result of tab/screen.
type screenResult struct {
	Cols  uint16   `json:"cols"`
	Rows  uint16   `json:"rows"`
	Lines []string `json:"lines"`
}

// scrollbackResult is the result of tab/scrollback. Total is the
// daemon-side total scrollback length so the caller can see how
// many more rows of history exist beyond the slice returned.
type scrollbackResult struct {
	From  int      `json:"from"`
	To    int      `json:"to"`
	Total int      `json:"total"`
	Lines []string `json:"lines"`
}

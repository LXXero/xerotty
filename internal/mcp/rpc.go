package mcp

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/LXXero/xerotty/internal/screentext"
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
//
// The activity fields let an agent tell a hung tab (input recent,
// output stale) from a live one, and a fresh tab from one abandoned
// weeks ago. Both an absolute time (RFC3339, spans any range cleanly)
// and an age in ms (server-computed, so the agent needn't trust its
// own clock) are given; the agent uses whichever fits.
type tabSummary struct {
	ID             uint32 `json:"id"`
	Name           string `json:"name,omitempty"`
	Title          string `json:"title"`
	Cols           uint16 `json:"cols"`
	Rows           uint16 `json:"rows"`
	WindowID       uint32 `json:"window_id"`
	Focused        bool   `json:"focused"`
	LastOutput      string `json:"last_output"`         // RFC3339 of most recent PTY output
	LastInput       string `json:"last_input"`          // RFC3339 of most recent input written
	LastOutputAgeMs int64  `json:"last_output_age_ms"`  // ms since last output (hung: this climbs while input is recent)
	LastInputAgeMs  int64  `json:"last_input_age_ms"`   // ms since last input
	LastActivity    string `json:"last_activity"`       // RFC3339 of the more recent of output/input
	IdleMs          int64  `json:"idle_ms"`             // ms since ANY activity (min of the two ages) — staleness sort
}

// tabIDParam decodes a tab_id that agents express sloppily: a JSON
// number (canonical), a numeric string ("3" — common agent slip), or
// — the important teaching case — a NAMESPACED id like "local:3",
// which belongs to the GUI aggregator's schema, not a single
// daemon's. Namespaced ids are REJECTED with an error explaining the
// schema split rather than a cryptic type failure: silently taking
// the numeric tail could target the wrong tab entirely (the "kh:3"
// an agent cached from the aggregator is NOT this daemon's tab 3).
type tabIDParam uint32

func (t *tabIDParam) UnmarshalJSON(b []byte) error {
	var n uint32
	if err := json.Unmarshal(b, &n); err == nil {
		*t = tabIDParam(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("tab_id must be a number")
	}
	if strings.Contains(s, ":") {
		return fmt.Errorf("tab_id %q is a NAMESPACED id from the GUI aggregator socket; this is a single daemon whose ids are plain integers — call list_tabs here and use those ids", s)
	}
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return fmt.Errorf("tab_id %q is not a number", s)
	}
	*t = tabIDParam(v)
	return nil
}

// screenResult is the result of tab/screen. Lines XOR Runs is set,
// depending on the request's styled flag.
type screenResult struct {
	Cols   uint16             `json:"cols"`
	Rows   uint16             `json:"rows"`
	Cursor cursorResult       `json:"cursor"`
	Lines  []string           `json:"lines,omitempty"`
	Runs   [][]screentext.Run `json:"runs,omitempty"`
}

// cursorResult is the cursor block in screenResult. The position is
// a strong presentation signal for agents: faint text at/after the
// cursor is typically a TUI's ghost-text suggestion, not real input.
type cursorResult struct {
	Row     int  `json:"row"`
	Col     int  `json:"col"`
	Visible bool `json:"visible"`
}

// scrollbackResult is the result of tab/scrollback. Total is the
// daemon-side total scrollback length so the caller can see how
// many more rows of history exist beyond the slice returned.
type scrollbackResult struct {
	From  int                `json:"from"`
	To    int                `json:"to"`
	Total int                `json:"total"`
	Lines []string           `json:"lines,omitempty"`
	Runs  [][]screentext.Run `json:"runs,omitempty"`
}

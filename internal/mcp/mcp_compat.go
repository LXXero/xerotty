package mcp

import (
	"encoding/json"
)

// This file adds Model Context Protocol (MCP) compliance on top of
// the existing native methods. MCP's standard surface is:
//
//   initialize        — handshake; client + server exchange
//                       protocol versions + capabilities
//   tools/list        — return the tools this server offers
//   tools/call        — invoke one of those tools with arguments
//
// The native methods (tabs/list, tab/screen, etc.) stay reachable
// directly for simple `nc -U` debugging — tools/call is just an
// MCP-shaped wrapper that dispatches to the same handler set.
//
// Spec reference: https://spec.modelcontextprotocol.io/

const mcpProtocolVersion = "2025-06-18"

// handleMCPInitialize answers the standard MCP handshake. We
// advertise tools/ capability + our server info. No prompts/resources
// for this phase.
func (c *agentConn) handleMCPInitialize(req *rpcRequest) *rpcResponse {
	return ok(req.ID, map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    "xerotty",
			"version": "0.1.0",
		},
	})
}

// handleMCPToolsList returns the tool catalog. Each tool maps 1:1
// to one of our native methods; tools/call below dispatches the
// arguments to the matching handler.
func (c *agentConn) handleMCPToolsList(req *rpcRequest) *rpcResponse {
	tools := []map[string]any{
		{
			"name":        "list_tabs",
			"description": "List every open tab in the daemon's default session. Returns id, title, dimensions, window membership, and whether the tab is focused.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			"name":        "get_screen",
			"description": "Read the visible viewport of a tab as plain text. Returns the rendered lines with trailing whitespace trimmed.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"tab_id": map[string]any{"type": "integer", "description": "ID from list_tabs"},
				},
				"required": []string{"tab_id"},
			},
		},
		{
			"name":        "get_scrollback",
			"description": "Read scrollback history (rows that scrolled off the viewport) as plain text. Defaults to the most recent 200 rows; pass {from, to} for an absolute range (0 = oldest). Result includes the total scrollback length so the agent can paginate.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"tab_id": map[string]any{"type": "integer"},
					"from":   map[string]any{"type": "integer", "description": "Absolute scrollback row index (0 = oldest). Omit for default tail."},
					"to":     map[string]any{"type": "integer", "description": "Exclusive upper bound. Omit for default tail."},
				},
				"required": []string{"tab_id"},
			},
		},
		{
			"name":        "send_input",
			"description": "Write raw bytes to a tab's PTY (keystrokes, escape sequences). Blocked in observe mode; use set_agent_mode to switch to propose or auto first.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"tab_id": map[string]any{"type": "integer"},
					"bytes":  map[string]any{"type": "string", "description": "Raw bytes as a UTF-8 string. Use \"\\r\" to submit a shell command."},
				},
				"required": []string{"tab_id", "bytes"},
			},
		},
		{
			"name":        "send_paste",
			"description": "Paste text into a tab. The daemon wraps it in bracketed-paste escapes when the foreground app enabled DECSET 2004.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"tab_id": map[string]any{"type": "integer"},
					"text":   map[string]any{"type": "string"},
				},
				"required": []string{"tab_id", "text"},
			},
		},
		{
			"name":        "create_tab",
			"description": "Spawn a new tab on the daemon. Defaults to 80x24, the daemon's default window, and the daemon's CWD. Returns the new tab_id. Blocked in observe mode.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"window_id": map[string]any{"type": "integer", "description": "Daemon window to put the tab in. 0 or omitted = default window."},
					"cols":      map[string]any{"type": "integer"},
					"rows":      map[string]any{"type": "integer"},
					"cwd":       map[string]any{"type": "string", "description": "Starting directory for the shell."},
				},
			},
		},
		{
			"name":        "close_tab",
			"description": "Close a tab, killing its PTY child. Blocked in observe mode.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"tab_id": map[string]any{"type": "integer"},
				},
				"required": []string{"tab_id"},
			},
		},
		{
			"name":        "list_proposals",
			"description": "List writes queued by agents in propose mode. Each entry has {index, tab_id, kind:\"input\"|\"paste\", payload}. The queue is bounded; older entries get dropped on overflow.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "approve_proposal",
			"description": "Apply the proposal at the given index to its target tab. Index comes from list_proposals. Indices shift after each approve/drop — re-list before each operation.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"index": map[string]any{"type": "integer"},
				},
				"required": []string{"index"},
			},
		},
		{
			"name":        "drop_proposal",
			"description": "Discard the proposal at the given index without applying.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"index": map[string]any{"type": "integer"},
				},
				"required": []string{"index"},
			},
		},
		{
			"name":        "resize_tab",
			"description": "Resize a tab's grid. Daemon updates the PTY winsize so foreground apps (vim, less, etc.) reflow. Blocked in observe mode.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"tab_id": map[string]any{"type": "integer"},
					"cols":   map[string]any{"type": "integer"},
					"rows":   map[string]any{"type": "integer"},
				},
				"required": []string{"tab_id", "cols", "rows"},
			},
		},
		{
			"name":        "get_clipboard",
			"description": "Get the session's most recently observed clipboard text.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			"name":        "set_agent_mode",
			"description": "Change this connection's mode: observe (read-only, default), propose (writes queue for user approval — no consumer yet, so they vanish), auto (writes apply directly).",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"mode": map[string]any{"type": "string", "enum": []string{"observe", "propose", "auto"}},
				},
				"required": []string{"mode"},
			},
		},
		{
			"name":        "list_clients",
			"description": "List every wire-protocol client currently attached to the daemon. Useful for seeing who else is driving the session.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			"name":        "get_server_info",
			"description": "Return the daemon's hostname + pid.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
	return ok(req.ID, map[string]any{"tools": tools})
}

// handleMCPToolsCall extracts {name, arguments} and dispatches to
// the underlying native-method handler. Wraps the response in
// MCP's "content" array shape: each tool result is a single
// text-typed content block carrying the JSON-serialized result so
// agents can json.Parse it on receipt.
func (c *agentConn) handleMCPToolsCall(req *rpcRequest) *rpcResponse {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return invalidParams(req.ID, err.Error())
	}

	// Reuse the native handlers by faking an rpcRequest with the
	// extracted arguments in the params slot. The result we get
	// back gets wrapped in MCP's content envelope.
	inner := &rpcRequest{
		JSONRPC: "2.0",
		ID:      req.ID,
		Params:  p.Arguments,
	}
	var resp *rpcResponse
	switch p.Name {
	case "list_tabs":
		resp = c.handleTabsList(inner)
	case "get_screen":
		resp = c.handleTabScreen(inner)
	case "get_scrollback":
		resp = c.handleTabScrollback(inner)
	case "send_input":
		resp = c.handleTabInput(inner)
	case "send_paste":
		resp = c.handleTabPaste(inner)
	case "create_tab":
		resp = c.handleTabCreate(inner)
	case "close_tab":
		resp = c.handleTabClose(inner)
	case "resize_tab":
		resp = c.handleTabResize(inner)
	case "list_proposals":
		resp = c.handleProposalsList(inner)
	case "approve_proposal":
		resp = c.handleProposalsApprove(inner)
	case "drop_proposal":
		resp = c.handleProposalsDrop(inner)
	case "get_clipboard":
		resp = c.handleClipboard(inner)
	case "set_agent_mode":
		resp = c.handleAgentMode(inner)
	case "list_clients":
		resp = c.handleAgentClients(inner)
	case "get_server_info":
		resp = c.handleServerInfo(inner)
	default:
		return rpcErr(req.ID, -32601, "tools/call: unknown tool "+p.Name, nil)
	}

	// If the inner handler returned an error, propagate it
	// directly (MCP convention: tool-execution errors come back
	// as a regular RPC error, not a content block with isError).
	if resp == nil {
		return rpcErr(req.ID, -32603, "tool returned nil response", nil)
	}
	if resp.Error != nil {
		return resp
	}

	// Wrap the JSON result in MCP's content envelope.
	return ok(req.ID, map[string]any{
		"content": []map[string]any{
			{
				"type": "text",
				"text": string(resp.Result),
			},
		},
		"isError": false,
	})
}

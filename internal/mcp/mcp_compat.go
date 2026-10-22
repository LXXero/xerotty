package mcp

import (
	"encoding/json"

	"github.com/LXXero/xerotty/internal/tabevents"
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
			// listChanged: the BRIDGE emits the notification after a
			// reconnect (a daemon upgrade may have changed the tool
			// catalog); declaring the capability is what makes the
			// client honor it.
			"tools": map[string]any{"listChanged": true},
		},
		"serverInfo": map[string]any{
			"name":    "xerotty",
			"version": "0.1.0",
		},
		// The spec's usage briefing — clients inject this into the
		// agent's context, so the trust-mode dance is known BEFORE
		// the first blocked write instead of discovered through
		// error archaeology.
		"instructions": "xerotty terminal daemon (single-daemon socket). You control real terminal tabs (PTYs with running shells) on THIS daemon only; tab ids are plain integers from list_tabs. Each tab's `label` is the text on the user's tab bar: when talking to the user, refer to a tab by its label; use tab_id only in tool calls — the user never sees ids. xerotty also has a separate GUI-aggregator MCP (serverInfo name 'xerotty-gui') with namespaced string ids like 'local:3' spanning every host the user's GUI is attached to — which server you are connected to was fixed when this connection was created and cannot be switched mid-session; if you need the other view, ask the user to register it (`xerotty mcp` = GUI view, `xerotty mcp --daemon` = this view).\n\n" +
			"TRUST MODE: every connection starts in '" + c.srv.defaultMode + "' mode. In observe mode all writes " +
			"(send_input, send_keys, create_tab, ...) are blocked. To write, FIRST call set_agent_mode with " +
			"{\"mode\":\"auto\"} (direct writes) or {\"mode\":\"propose\"} (writes queue for human approval). " +
			"If the connection ever drops and reconnects, mode may reset — on an unexpected 'observe mode' error, " +
			"call set_agent_mode again.\n\n" +
			"TIPS: use send_keys (named keys: enter, ctrl+c, arrows — e.g. {\"text\":\"ls\",\"keys\":[\"enter\"]}) " +
			"instead of raw escape bytes in send_input. Pass a stable `name` to create_tab to reuse a tab instead of " +
			"stacking duplicates. get_screen always returns cursor {row,col}; styled=true returns styled runs — " +
			"faint text at/after the cursor is a TUI's autocomplete ghost text, NOT user input. " +
			"To find out when something happens in a tab (an agent finishes or goes idle, a command exits, a bell, a " +
			"desktop notification), call wait_for_events in a loop instead of polling get_screen.",
	})
}

// handleMCPToolsList returns the tool catalog. Each tool maps 1:1
// to one of our native methods; tools/call below dispatches the
// arguments to the matching handler.
func (c *agentConn) handleMCPToolsList(req *rpcRequest) *rpcResponse {
	tools := []map[string]any{
		{
			"name":        "list_tabs",
			"description": "List every open tab in the daemon's default session. Returns id, label (the text on the user's tab bar: assigned name, else program title without status glyphs, else foreground process — refer to tabs by label when talking to the user, and use the id only in tool calls), name (the reuse label, if any), title, dimensions, window membership, and whether the tab is focused. Check here for an existing named tab before calling create_tab.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			"name":        "get_screen",
			"description": "Read the visible viewport of a tab. Always includes cursor {row, col, visible}. Default returns plain-text lines (trailing whitespace trimmed); styled=true returns runs of styled text per line ({t: text, fg/bg: palette int or #rrggbb, a: attrs like \"faint\" or \"bold,underline\"}) — use it to tell presentation from content: faint text at/after the cursor is typically a TUI's autocomplete ghost-text suggestion the user has NOT typed; red fg usually marks errors.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"tab_id": map[string]any{"type": "integer", "description": "ID from list_tabs"},
					"styled": map[string]any{"type": "boolean", "description": "Return styled runs instead of flat lines."},
				},
				"required": []string{"tab_id"},
			},
		},
		{
			"name":        "get_scrollback",
			"description": "Read scrollback history (rows that scrolled off the viewport). Defaults to the most recent 200 rows; pass {from, to} for an absolute range (0 = oldest). Result includes the total scrollback length so the agent can paginate. styled=true returns styled runs instead of flat lines (see get_screen).",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"tab_id": map[string]any{"type": "integer"},
					"from":   map[string]any{"type": "integer", "description": "Absolute scrollback row index (0 = oldest). Omit for default tail."},
					"to":     map[string]any{"type": "integer", "description": "Exclusive upper bound. Omit for default tail."},
					"styled": map[string]any{"type": "boolean", "description": "Return styled runs instead of flat lines."},
				},
				"required": []string{"tab_id"},
			},
		},
		{
			"name":        "send_input",
			"description": "Write raw bytes to a tab's PTY. The string is used as-is after standard JSON unescaping — no extra escape layer. Prefer send_keys for keystrokes (enter, ctrl+c, arrows): it cannot be mis-escaped. Blocked in observe mode; use set_agent_mode to switch to propose or auto first.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"tab_id": map[string]any{"type": "integer"},
					"bytes":  map[string]any{"type": "string", "description": "Raw bytes as a UTF-8 string."},
				},
				"required": []string{"tab_id", "bytes"},
			},
		},
		{
			"name":        "send_keys",
			"description": "Press keys by NAME — use this instead of guessing raw byte escapes for send_input (sending Enter as \\r/\\n escape soup is a known failure loop; here it is just \"enter\"). Optional `text` is sent first, completely literally (no escape interpretation), then each `keys` token is pressed in order. When the app turned on bracketed paste (DECSET 2004), the text is sent as a paste (wrapped in paste markers) so a trailing enter still submits; otherwise it is typed. Tokens: a single literal character, or a named key (enter, esc, tab, backspace, space, delete, insert, up, down, left, right, home, end, pageup, pagedown, f1-f12), with optional modifier prefixes joined by + or - (ctrl+c, alt+enter, ctrl+shift+up, ctrl++ = ctrl and '+'; tmux-style C-c / M-x also accepted). Arrows honor the tab's app-cursor mode automatically. Example: run a command = {text: \"ls\", keys: [\"enter\"]}; interrupt = {keys: [\"ctrl+c\"]}. Blocked in observe mode.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"tab_id": map[string]any{"type": "integer"},
					"text":   map[string]any{"type": "string", "description": "Literal text sent before the keys (pasted when the app enabled bracketed paste)."},
					"keys":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Key tokens pressed in order."},
				},
				"required": []string{"tab_id"},
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
			"description": "Open a tab on the daemon, reusing one when possible. Pass a stable `name` to make this idempotent: the first call spawns and labels the tab, later calls with the same name return that same tab (response field reused=true) instead of stacking duplicates — prefer this over spawning a fresh tab each time. Omit name for a throwaway one-off tab. Defaults to 80x24, the daemon's default window, and the daemon's CWD. Returns tab_id, name, and reused. Blocked in observe mode.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":      map[string]any{"type": "string", "description": "Stable reuse label (e.g. \"build\", \"logs\"). Same name returns the same tab. Omit for a one-off tab."},
					"window_id": map[string]any{"type": "integer", "description": "Daemon window to put the tab in. 0 or omitted = default window."},
					"cols":      map[string]any{"type": "integer"},
					"rows":      map[string]any{"type": "integer"},
					"cwd":       map[string]any{"type": "string", "description": "Starting directory for the shell. Ignored when reusing an existing named tab."},
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
			"name":        "rename_tab",
			"description": "Set a tab's name, the label shown on the user's tab bar. An empty name clears it, so the tab shows its program's title again. Names are also create_tab's reuse key: clearing or changing a name means a later create_tab with the old name opens a new tab. Fails if another tab already has the name. Blocked in observe mode.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"tab_id": map[string]any{"type": "integer"},
					"name":   map[string]any{"type": "string", "description": "New name; \"\" clears it."},
				},
				"required": []string{"tab_id", "name"},
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
			"description": "Change this connection's mode: observe (read-only, default), propose (writes queue for review — inspect with list_proposals, an authorized reviewer applies via approve_proposal or discards via drop_proposal), auto (writes apply directly). If the daemon has allow_mode_change=false you can only de-escalate unless you authenticate.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"mode": map[string]any{"type": "string", "enum": []string{"observe", "propose", "auto"}},
				},
				"required": []string{"mode"},
			},
		},
		{
			"name":        "authenticate",
			"description": "Present the daemon's approval token to gain approval authority. Required for approve_proposal / drop_proposal (and to elevate to auto) when the daemon runs with allow_mode_change=false. This is how a generic MCP client becomes the trusted reviewer in a propose-mode gate.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"token": map[string]any{"type": "string"},
				},
				"required": []string{"token"},
			},
		},
		{
			"name":        "list_clients",
			"description": "List every wire-protocol client currently attached to the daemon: client id, remote address, session, join time, seconds since its last heartbeat pong (a growing number means a half-dead client, e.g. a sleeping laptop's still-open SSH), and how many tabs it watches.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			"name":        "disconnect_client",
			"description": "Force-disconnect an attached client by its ClientID (from list_clients). The shared grid hands off to the remaining clients as if it dropped on its own. Use on half-dead remote clients squatting on the session. Blocked in observe mode.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"client_id": map[string]any{"type": "string"},
				},
				"required": []string{"client_id"},
			},
		},
		{
			"name":        "wait_for_events",
			"description": eventsToolDescription,
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"after_seq":  map[string]any{"type": "integer", "description": "Return events with seq greater than this: the last_seq of the previous reply. 0 = everything still in the ring."},
					"epoch":      map[string]any{"type": "string", "description": "The epoch of the previous reply. Lets the daemon tell that after_seq came from an earlier daemon process (restart or upgrade); omit on the first call."},
					"timeout_ms": map[string]any{"type": "integer", "description": "How long to block for a matching event. Default 30000, max 300000; 0 returns at once."},
					"tab_ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "Only events for these tabs (ids from list_tabs). Omit for all tabs."},
					"kinds":      map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": eventKindNames()}, "description": "Only these event kinds. Omit for all kinds."},
				},
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
	case "send_keys":
		resp = c.handleTabKeys(inner)
	case "send_paste":
		resp = c.handleTabPaste(inner)
	case "create_tab":
		resp = c.handleTabCreate(inner)
	case "close_tab":
		resp = c.handleTabClose(inner)
	case "rename_tab":
		resp = c.handleTabRename(inner)
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
	case "authenticate":
		resp = c.handleAgentAuthenticate(inner)
	case "list_clients":
		resp = c.handleAgentClients(inner)
	case "disconnect_client":
		resp = c.handleAgentKick(inner)
	case "get_server_info":
		resp = c.handleServerInfo(inner)
	case "wait_for_events":
		resp = c.handleEventsWait(inner)
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

// eventsToolDescription is wait_for_events' tool description.
const eventsToolDescription = "Wait for something to happen in this daemon's tabs instead of polling screens. Returns the events after after_seq, or blocks until one arrives or timeout_ms passes (then events is empty). " + tabevents.KindsDescription + " Each reply has last_seq and epoch: pass them back as after_seq and epoch on the next call. missed=true means events after your cursor are gone: missed_reason \"restarted\" (the daemon restarted, so the cursor is from an earlier process) or \"overflow\" (more events than the ring holds happened since your last call); the reply then holds everything still in the ring. Allowed in observe mode."

// eventKindNames lists the event kinds for the tool schema.
func eventKindNames() []string {
	out := make([]string, len(tabevents.AllKinds))
	for i, k := range tabevents.AllKinds {
		out[i] = string(k)
	}
	return out
}

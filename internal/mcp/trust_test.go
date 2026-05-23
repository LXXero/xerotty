package mcp_test

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
	"github.com/LXXero/xerotty/internal/mcp"
)

// TestMCPTrustBoundary exercises the propose-as-gate config:
// default_mode=propose, allow_mode_change=false, approval_token set.
// An unauthenticated connection lands in propose, can't elevate to
// auto, and can't approve. A token-authenticated connection can.
func TestMCPTrustBoundary(t *testing.T) {
	dir := t.TempDir()
	wireSock := filepath.Join(dir, "xerottyd.sock")
	mcpSock := filepath.Join(dir, "xerottyd.mcp.sock")

	cfg := config.Default()
	cfg.MCP.DefaultMode = "propose"
	cfg.MCP.AllowModeChange = false
	cfg.MCP.ApprovalToken = "s3cret"

	d := daemon.New(&cfg, wireSock)
	doneRun := make(chan error, 1)
	go func() { doneRun <- d.Run() }()
	defer func() { _ = d.Stop(); <-doneRun }()

	srv := mcp.New(d, mcpSock)
	doneMCP := make(chan error, 1)
	go func() { doneMCP <- srv.Run() }()
	defer func() { _ = srv.Stop(); <-doneMCP }()
	time.Sleep(80 * time.Millisecond)

	// --- unauthenticated agent ---
	agent := dialMCP(t, mcpSock)
	defer agent.Close()

	// Starts in propose (the configured default).
	modeRes := agent.call(t, 1, "agent/mode", nil)
	var m struct {
		Mode string `json:"mode"`
	}
	json.Unmarshal(modeRes, &m)
	if m.Mode != "propose" {
		t.Fatalf("default mode: got %q want propose", m.Mode)
	}

	// Can't elevate to auto under the lock.
	errResp := agent.callExpectErr(t, 2, "agent/mode", map[string]any{"mode": "auto"})
	if errResp.Code != -32099 {
		t.Errorf("elevate-to-auto: got code %d want -32099 (%s)", errResp.Code, errResp.Message)
	}

	// Can't approve (no auto, no token).
	errResp = agent.callExpectErr(t, 3, "proposals/approve", map[string]any{"index": 0})
	if errResp.Code != -32099 {
		t.Errorf("unauthed approve: got code %d want -32099", errResp.Code)
	}

	// --- reviewer authenticates with the token ---
	reviewer := dialMCP(t, mcpSock)
	defer reviewer.Close()

	bad := reviewer.callExpectErr(t, 1, "agent/authenticate", map[string]any{"token": "wrong"})
	if bad.Code != -32099 {
		t.Errorf("bad token: got code %d want -32099", bad.Code)
	}

	authRes := reviewer.call(t, 2, "agent/authenticate", map[string]any{"token": "s3cret"})
	var a struct {
		Authenticated bool `json:"authenticated"`
	}
	json.Unmarshal(authRes, &a)
	if !a.Authenticated {
		t.Fatalf("authenticate with correct token failed")
	}

	// Authed reviewer can approve even at default mode (empty
	// queue → "no proposal at index" is the expected error, NOT
	// the -32099 authority error).
	approveErr := reviewer.callExpectErr(t, 3, "proposals/approve", map[string]any{"index": 0})
	if approveErr.Code == -32099 {
		t.Errorf("authed reviewer was denied approval authority: %s", approveErr.Message)
	}
}

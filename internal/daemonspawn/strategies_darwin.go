package daemonspawn

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/LXXero/xerotty/internal/sockpath"
)

func strategies() []strategy {
	return []strategy{launchdGUI, forkStrategy}
}

// launchdGUI bootstraps the daemon as a launchd job in the user's gui
// domain. That is macOS's equivalent of the systemd user scope: the
// job belongs to the console login's Aqua session, so a daemon
// spawned from an ssh shell still reaches the pasteboard and
// WindowServer, and it outlives the ssh session like any other
// per-user agent. A plain fork from ssh lands in that ssh session's
// bootstrap namespace instead, where none of that works.
//
// The plist is transient: bootstrapped from the cache dir and
// removed right after, so nothing is registered at the next login.
// AbandonProcessGroup keeps launchd from killing the tab shells when
// the daemon process exits (crash, `serve --upgrade` fallback).
var launchdGUI = strategy{
	name: "launchd gui domain",
	available: func() bool {
		if _, err := exec.LookPath("launchctl"); err != nil {
			return false
		}
		// No console login → no gui domain → bootstrap would fail.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return exec.CommandContext(ctx, "launchctl", "print", guiDomain()).Run() == nil
	},
	start: func(o Options, env []string) (handle, error) {
		label := "com.github.lxxero." + UnitName(o.SocketPath)
		dir, err := sockpath.CacheDir()
		if err != nil {
			return handle{}, err
		}
		dir = filepath.Join(dir, "launchd")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return handle{}, err
		}
		plistPath := filepath.Join(dir, label+".plist")
		plist := launchdPlist(label, []string{o.Self, "serve", "--socket", o.SocketPath}, env, sockpath.DaemonLogPath())
		if err := os.WriteFile(plistPath, plist, 0o600); err != nil {
			return handle{}, err
		}
		defer os.Remove(plistPath)

		var stderr bytes.Buffer
		cmd := exec.Command("launchctl", "bootstrap", guiDomain(), plistPath)
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return handle{}, fmt.Errorf("launchctl bootstrap: %v: %s", err, strings.TrimSpace(stderr.String()))
		}
		return handle{
			// launchd owns the process; we can't wait on it.
			exited: nil,
			abort: func() {
				_ = exec.Command("launchctl", "bootout", guiDomain()+"/"+label).Run()
			},
		}, nil
	},
}

func guiDomain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

// launchdPlist renders a minimal launchd job: run once at load, don't
// restart, log stderr to logPath, and leave child processes alone
// when the job's own process exits.
func launchdPlist(label string, argv, env []string, logPath string) []byte {
	var b bytes.Buffer
	esc := func(s string) string {
		var e bytes.Buffer
		_ = xml.EscapeText(&e, []byte(s))
		return e.String()
	}
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString("<plist version=\"1.0\"><dict>\n")
	fmt.Fprintf(&b, "  <key>Label</key><string>%s</string>\n", esc(label))
	b.WriteString("  <key>ProgramArguments</key><array>\n")
	for _, a := range argv {
		fmt.Fprintf(&b, "    <string>%s</string>\n", esc(a))
	}
	b.WriteString("  </array>\n")
	if len(env) > 0 {
		b.WriteString("  <key>EnvironmentVariables</key><dict>\n")
		for _, kv := range env {
			k, v, ok := strings.Cut(kv, "=")
			if !ok || k == "" {
				continue
			}
			fmt.Fprintf(&b, "    <key>%s</key><string>%s</string>\n", esc(k), esc(v))
		}
		b.WriteString("  </dict>\n")
	}
	if logPath != "" {
		fmt.Fprintf(&b, "  <key>StandardErrorPath</key><string>%s</string>\n", esc(logPath))
	}
	b.WriteString("  <key>StandardOutPath</key><string>/dev/null</string>\n")
	b.WriteString("  <key>RunAtLoad</key><true/>\n")
	b.WriteString("  <key>KeepAlive</key><false/>\n")
	b.WriteString("  <key>AbandonProcessGroup</key><true/>\n")
	b.WriteString("</dict></plist>\n")
	return b.Bytes()
}

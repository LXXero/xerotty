// Package menu implements the config-driven right-click context menu.
package menu

import (
	"os"
	"os/exec"
	"strings"

	"github.com/AllenDang/cimgui-go/imgui"
	"github.com/LXXero/xerotty/internal/config"
)

// Context holds runtime state for menu condition evaluation.
type Context struct {
	HasSelection bool
	HasLink      bool
	Selection    string
	Link         string
	CWD          string
	TabTitle     string
}

// Render draws the context menu using ImGui. Returns the action to dispatch, or "".
func Render(items []config.MenuItem, ctx *Context) string {
	if !imgui.BeginPopupContextWindowV("", imgui.PopupFlagsMouseButtonRight) {
		return ""
	}
	defer imgui.EndPopup()

	action := renderItems(items, ctx)
	return action
}

func renderItems(items []config.MenuItem, ctx *Context) string {
	for _, item := range items {
		// Check enabled condition
		if !checkEnabled(item.Enabled, ctx) {
			continue
		}

		if item.Action == "separator" {
			imgui.Separator()
			continue
		}

		// Submenu
		if len(item.Submenu) > 0 {
			if imgui.BeginMenu(item.Label) {
				if action := renderItems(item.Submenu, ctx); action != "" {
					imgui.EndMenu()
					return action
				}
				imgui.EndMenu()
			}
			continue
		}

		// Regular item
		if imgui.MenuItemBoolV(item.Label, item.Shortcut, false, true) {
			return item.Action
		}
	}
	return ""
}

func checkEnabled(condition string, ctx *Context) bool {
	switch condition {
	case "has_selection":
		return ctx.HasSelection
	case "has_link":
		return ctx.HasLink
	case "in_tmux":
		return os.Getenv("TMUX") != ""
	case "", "always":
		return true
	}
	return true
}

// ExecAction executes a shell hook action (exec:command).
func ExecAction(action string, ctx *Context) {
	if !strings.HasPrefix(action, "exec:") {
		return
	}

	cmdStr := strings.TrimPrefix(action, "exec:")

	// Variable substitution
	cmdStr = strings.ReplaceAll(cmdStr, "$XEROTTY_SELECTION", ctx.Selection)
	cmdStr = strings.ReplaceAll(cmdStr, "$XEROTTY_LINK", ctx.Link)
	cmdStr = strings.ReplaceAll(cmdStr, "$XEROTTY_CWD", ctx.CWD)
	cmdStr = strings.ReplaceAll(cmdStr, "$XEROTTY_TAB_TITLE", ctx.TabTitle)

	cmd := exec.Command("/bin/sh", "-c", cmdStr)
	cmd.Start()
}

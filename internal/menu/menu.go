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

// Render draws the context menu at desktop coordinates (x, y). Caller
// is responsible for tracking open/close state — Render just paints
// when invoked. Returns the action a clicked item produced (empty if
// none) and a close hint set true when the menu should be dismissed
// (Escape pressed, or a click landed outside the menu's bounds).
//
// We use BeginV here rather than ImGui's BeginPopup because the
// popup machinery is wired to the host viewport's focus lifecycle
// under multi-viewport: any time the cursor crossed the parent OS
// window edge, ImGui snapped the popup shut as "clicked outside",
// even when the user was just moving toward the menu. With BeginV
// and our own click-out detection the menu stays put until the user
// explicitly clicks elsewhere or hits Escape.
//
// NoAutoMerge + TopMost make the menu its own OS window so it can
// extend past the parent's bounds (matching native context-menu
// behavior). Set immediately before BeginV — window class only
// applies to the next Begin call.
func Render(items []config.MenuItem, ctx *Context, x, y float32, allowCloseClick bool) (string, bool, uintptr) {
	wc := imgui.NewWindowClass()
	wc.SetViewportFlagsOverrideSet(imgui.ViewportFlagsNoAutoMerge | imgui.ViewportFlagsTopMost)
	imgui.SetNextWindowClass(wc)
	wc.Destroy()
	imgui.SetNextWindowPosV(imgui.Vec2{X: x, Y: y}, imgui.CondAppearing, imgui.Vec2{X: 0, Y: 0})

	flags := imgui.WindowFlagsNoTitleBar |
		imgui.WindowFlagsNoResize |
		imgui.WindowFlagsNoMove |
		imgui.WindowFlagsNoSavedSettings |
		imgui.WindowFlagsNoDocking |
		imgui.WindowFlagsAlwaysAutoResize |
		imgui.WindowFlagsNoCollapse

	var action string
	var close bool
	var handle uintptr
	if imgui.BeginV("##contextmenu", nil, flags) {
		if imgui.WindowViewport() != nil {
			handle = imgui.WindowViewport().PlatformHandle()
		}
		menuPos := imgui.WindowPos()
		menuSize := imgui.WindowSize()
		mousePos := imgui.MousePos()
		mouseInsideMenu := mousePos.X >= menuPos.X &&
			mousePos.X < menuPos.X+menuSize.X &&
			mousePos.Y >= menuPos.Y &&
			mousePos.Y < menuPos.Y+menuSize.Y
		action = renderItems(items, ctx)

		// Close on Escape.
		if imgui.IsKeyPressedBool(imgui.KeyEscape) {
			close = true
		}
		// Outside click closes the menu. Do not close on mouse-down
		// inside the menu: Selectable normally activates on release,
		// so closing on the initial down event destroys the menu before
		// the item can return an action.
		if allowCloseClick {
			anyClick := imgui.IsMouseClickedBool(imgui.MouseButtonLeft) ||
				imgui.IsMouseClickedBool(imgui.MouseButtonRight) ||
				imgui.IsMouseClickedBool(imgui.MouseButtonMiddle)
			if anyClick && !mouseInsideMenu {
				close = true
			}
		}
	}
	imgui.End()

	return action, close, handle
}

func renderItems(items []config.MenuItem, ctx *Context) string {
	for _, item := range items {
		enabled := checkEnabled(item.Enabled, ctx)

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

		// Regular item — render as Selectable rather than MenuItem.
		// MenuItem is designed for use inside BeginMenu/BeginPopup
		// scopes; in a plain BeginV (which we use so we can control
		// dismiss lifecycle manually) the click-activate behavior is
		// inconsistent — the click event reaches ImGui but MenuItem's
		// internal hover gate doesn't fire activation. Selectable is
		// the more primitive widget that just activates on click and
		// works reliably regardless of window flags. Lay out the
		// shortcut hint manually on the right edge to keep the same
		// visual.
		label := item.Label
		if item.Shortcut != "" {
			label = label + "  " + item.Shortcut
		}
		flags := imgui.SelectableFlags(0)
		if !enabled {
			flags |= imgui.SelectableFlagsDisabled
		}
		if imgui.SelectableBoolV(label, false, flags, imgui.Vec2{X: 0, Y: 0}) {
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

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
	// ForceOpaque reflects the live opacity-toggle state so a menu item
	// with Checked == "force_opaque" can show whether it's currently on.
	ForceOpaque bool
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

// RenderItemsOnly walks items and emits MenuItem / Separator / nested
// BeginMenu calls into the CURRENT ImGui window. No Begin/End wrapper
// — caller (e.g. platform.RunImGuiPopup's draw callback) owns the
// window. Returns the action of the clicked item, empty string for
// nothing-this-frame.
func RenderItemsOnly(items []config.MenuItem, ctx *Context) string {
	return renderItems(items, ctx)
}

func renderItems(items []config.MenuItem, ctx *Context) string {
	for _, item := range items {
		enabled := checkEnabled(item.Enabled, ctx)
		checked := checkChecked(item.Checked, ctx)

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
		// works reliably regardless of window flags.
		//
		// To match MenuItem's "shortcut at far right" visual we snapshot
		// the row's top-left + available width BEFORE Selectable runs,
		// let Selectable render its background + left-aligned label,
		// then overlay the shortcut via the window draw list positioned
		// at row_right - shortcut_width. Drawing through the draw list
		// (rather than SameLine + Text) keeps the cursor flow clean and
		// guarantees the shortcut sits on the same row regardless of
		// item height changes from style tweaks.
		rowStart := imgui.CursorScreenPos()
		availW := imgui.ContentRegionAvail().X
		flags := imgui.SelectableFlags(0)
		if !enabled {
			flags |= imgui.SelectableFlagsDisabled
		}
		sel := imgui.SelectableBoolV(item.Label, checked, flags, imgui.Vec2{X: 0, Y: 0})
		if item.Shortcut != "" {
			sw := imgui.CalcTextSize(item.Shortcut).X
			var col uint32
			if enabled {
				col = imgui.ColorU32Col(imgui.ColText)
			} else {
				col = imgui.ColorU32Col(imgui.ColTextDisabled)
			}
			imgui.WindowDrawList().AddTextVec2(
				imgui.Vec2{X: rowStart.X + availW - sw, Y: rowStart.Y},
				col, item.Shortcut)
		}
		if sel {
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

// checkChecked resolves a menu item's Checked predicate to its live
// on/off state, so renderItems can show the item as toggled on. Empty
// or unknown predicates are never checked.
func checkChecked(condition string, ctx *Context) bool {
	switch condition {
	case "force_opaque":
		return ctx.ForceOpaque
	}
	return false
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

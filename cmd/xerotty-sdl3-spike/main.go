// xerotty-sdl3-spike — Phase 1b acceptance binary.
//
// Spawns a shell, renders its output through internal/renderer into the
// new SDL3-backed platform layer, and feeds keyboard input back to the
// PTY. Single window, no tabs, no prefs — proves the renderer drops
// cleanly into the new frame callback. See docs/SDL3_PLAN.md.
package main

import (
	"fmt"
	"log"
	"os"
	"runtime"

	"github.com/AllenDang/cimgui-go/imgui"

	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/platform"
	"github.com/LXXero/xerotty/internal/renderer"
	"github.com/LXXero/xerotty/internal/terminal"
)

func main() {
	// Lock the main goroutine to the OS thread that ran SDL_Init / created
	// the GL context. SDL3 + OpenGL both have thread-affinity requirements
	// — if Go's scheduler reparks us on another OS thread, GL calls hit
	// an unbound context and the next swap segfaults.
	runtime.LockOSThread()

	cfg := config.Default()

	if err := platform.Init("xerotty SDL3 spike — terminal", 1024, 640); err != nil {
		log.Fatalf("platform.Init: %v", err)
	}
	defer platform.Shutdown()

	// Fonts must be added to the atlas BEFORE the first NewFrame —
	// cimgui-go's dynamic atlas builds itself the first time the font
	// is accessed during a frame, and adding fonts after that asserts.
	font, fontBold := renderer.LoadFont(&cfg)
	pxSize := renderer.PixelSize(&cfg)

	theme := renderer.DefaultTheme()
	r := renderer.New(theme, renderer.CellMetrics{
		Width: pxSize * 0.6, Height: pxSize, // placeholder — refined on first frame
	}, font, pxSize)
	r.FontBold = fontBold
	pad := float32(cfg.Appearance.Padding)
	r.OffsetX = pad
	r.OffsetY = pad
	r.BoldIsBright = cfg.Appearance.BoldIsBright

	// Start the shell with a reasonable initial size; we'll Resize once
	// real cell metrics are known after the first ImGui frame builds the
	// font atlas.
	cwd, _ := os.Getwd()
	term, err := terminal.New(&cfg, 80, 24, cwd)
	if err != nil {
		log.Fatalf("terminal.New: %v", err)
	}
	defer term.Close()

	// Wire the PTY-reader → main-loop wake. Without this, new output
	// from the shell wouldn't render until the next frame-cap timeout
	// elapses, which is choppy for fast-streaming output (cat, find, …).
	terminal.Wake = platform.PostWake

	measured := false

	for platform.Frame(func() {
		// One ImGui window covering the whole OS viewport — matches
		// what the existing app does with its "wrapper window". The
		// renderer's OffsetX/Y are in screen coords (DrawList API),
		// and a viewport-aligned window with NoMove keeps the wrapper
		// at (0,0) of the viewport so OffsetX/Y are just padding.
		vp := imgui.MainViewport()
		imgui.SetNextWindowPos(vp.Pos())
		imgui.SetNextWindowSize(vp.Size())
		flags := imgui.WindowFlagsNoTitleBar | imgui.WindowFlagsNoResize |
			imgui.WindowFlagsNoMove | imgui.WindowFlagsNoScrollbar |
			imgui.WindowFlagsNoCollapse | imgui.WindowFlagsNoBackground |
			imgui.WindowFlagsNoBringToFrontOnFocus |
			imgui.WindowFlagsNoSavedSettings

		if imgui.BeginV("xerotty-wrapper", nil, flags) {
			// First frame after the atlas is built: measure the real
			// glyph dimensions and resize the PTY so cols/rows match
			// what we can actually display. Doing this earlier (before
			// the atlas exists) gives wrong metrics.
			if !measured {
				m := renderer.MeasureCell()
				if m.Width > 0 && m.Height > 0 {
					r.Metrics = m
					ws := vp.Size()
					cols := int((ws.X - 2*pad) / m.Width)
					rows := int((ws.Y - 2*pad) / m.Height)
					if cols > 0 && rows > 0 {
						term.Resize(cols, rows)
					}
					fmt.Fprintf(os.Stderr,
						"spike: cell=%.1fx%.1f grid=%dx%d window=%.0fx%.0f\n",
						m.Width, m.Height, cols, rows, ws.X, ws.Y)
					measured = true
				}
			}

			drawList := imgui.WindowDrawList()
			r.Draw(term.Emu, drawList, 0)
		}
		imgui.End()

		// Keyboard input → PTY. Minimal Phase 1b set: printable
		// characters via ImGui's text input queue, plus the handful
		// of control keys you need to actually use a shell. Full
		// keybind / Alt-meta / arrow modes / mouse reporting live in
		// internal/app and don't need to come over until Phase 4.
		io := imgui.CurrentIO()
		ctrlHeld := imgui.IsKeyDown(imgui.ModCtrl)

		// Ctrl+letter → 0x01-0x1A. Has to be handled separately from
		// the text input queue: pressing Ctrl+C doesn't produce a
		// TEXT_INPUT event on most platforms (the OS withholds the
		// printable char while a modifier is held), so we synthesize
		// the control byte from the raw key event ourselves.
		if ctrlHeld {
			for k := imgui.KeyA; k <= imgui.KeyZ; k++ {
				if imgui.IsKeyPressedBool(k) {
					term.Write([]byte{byte(k-imgui.KeyA) + 1})
				}
			}
		} else {
			chars := io.InputQueueCharacters()
			if chars.Size > 0 {
				for _, ch := range chars.Slice() {
					if ch > 0 && ch < 0x10FFFF {
						buf := make([]byte, 4)
						n := encodeRune(buf, rune(ch))
						term.Write(buf[:n])
					}
				}
			}
		}
		if imgui.IsKeyPressedBool(imgui.KeyEnter) {
			term.Write([]byte("\r"))
		}
		if imgui.IsKeyPressedBool(imgui.KeyBackspace) {
			term.Write([]byte{0x7f})
		}
		if imgui.IsKeyPressedBool(imgui.KeyTab) {
			term.Write([]byte("\t"))
		}
		if imgui.IsKeyPressedBool(imgui.KeyEscape) {
			term.Write([]byte{0x1b})
		}

		// Right-click → open the SDL3 popup. Phase 2 acceptance: the
		// popup is a real xdg_popup on Wayland, dismisses on click
		// outside (incl over native Wayland apps), dismisses on
		// Escape, returns the clicked strip's index on selection.
		// Strips are colored solids for now — text rendering is the
		// next iteration once dismiss semantics are validated.
		if imgui.IsMouseClickedBool(imgui.MouseButtonRight) {
			pos := imgui.MousePos()
			const itemH = 28
			const numItems = 4
			sel := platform.RunPopup(int(pos.X), int(pos.Y),
				180, itemH*numItems, numItems)
			fmt.Fprintf(os.Stderr, "popup: result=%d\n", sel)
		}
	}) {
		if term.IsClosed() {
			break
		}
	}
}

func encodeRune(buf []byte, r rune) int {
	switch {
	case r < 0x80:
		buf[0] = byte(r)
		return 1
	case r < 0x800:
		buf[0] = byte(0xC0 | (r >> 6))
		buf[1] = byte(0x80 | (r & 0x3F))
		return 2
	case r < 0x10000:
		buf[0] = byte(0xE0 | (r >> 12))
		buf[1] = byte(0x80 | ((r >> 6) & 0x3F))
		buf[2] = byte(0x80 | (r & 0x3F))
		return 3
	default:
		buf[0] = byte(0xF0 | (r >> 18))
		buf[1] = byte(0x80 | ((r >> 12) & 0x3F))
		buf[2] = byte(0x80 | ((r >> 6) & 0x3F))
		buf[3] = byte(0x80 | (r & 0x3F))
		return 4
	}
}

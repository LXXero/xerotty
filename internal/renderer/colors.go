// Package renderer handles cell grid → ImGui draw commands.
package renderer

import (
	"image/color"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Theme holds resolved colors for rendering.
type Theme struct {
	Foreground      uint32
	Background      uint32
	Bold            uint32 // explicit bold-text color override; 0 = none
	Cursor          uint32
	SelectionFg     uint32
	SelectionBg     uint32
	ScrollbarBg     uint32
	ScrollbarThumb  uint32
	ScrollbarHover  uint32
	ANSI            [16]uint32
}

// DefaultTheme returns the Dracula theme.
func DefaultTheme() Theme {
	return Theme{
		Foreground:  0xFFF2F8F8, // #F8F8F2 in ABGR
		Background:  0xFF36282A, // #282A36 in ABGR (note: ABGR = 0xAABBGGRR)
		Cursor:         0xFFF2F8F8,
		SelectionFg:    0xFFF2F8F8,
		SelectionBg:    0xFF5A4744,
		ScrollbarBg:    hexToABGR("#282A36"),
		ScrollbarThumb: hexToABGR("#44475A"),
		ScrollbarHover: hexToABGR("#6272A4"),
		ANSI: [16]uint32{
			hexToABGR("#21222C"), // black
			hexToABGR("#FF5555"), // red
			hexToABGR("#50FA7B"), // green
			hexToABGR("#F1FA8C"), // yellow
			hexToABGR("#BD93F9"), // blue
			hexToABGR("#FF79C6"), // magenta
			hexToABGR("#8BE9FD"), // cyan
			hexToABGR("#F8F8F2"), // white
			hexToABGR("#6272A4"), // bright black
			hexToABGR("#FF6E6E"), // bright red
			hexToABGR("#69FF94"), // bright green
			hexToABGR("#FFFFA5"), // bright yellow
			hexToABGR("#D6ACFF"), // bright blue
			hexToABGR("#FF92DF"), // bright magenta
			hexToABGR("#A4FFFF"), // bright cyan
			hexToABGR("#FFFFFF"), // bright white
		},
	}
}

// ColorToABGR converts a color.Color to ImGui packed ABGR uint32.
func ColorToABGR(c color.Color) uint32 {
	if c == nil {
		return 0
	}
	r, g, b, a := c.RGBA()
	return (uint32(a>>8) << 24) | (uint32(b>>8) << 16) | (uint32(g>>8) << 8) | uint32(r>>8)
}

// ResolveColor resolves a terminal color to ABGR uint32 using the theme.
// If bold is true, the basic ANSI range (0-7) is promoted to bright (8-15),
// and a nil (default) fg is promoted to bright white (15) — matching xterm's
// "bold is bright" behavior used by xfce4-terminal/gnome-terminal.
func (t *Theme) ResolveColor(c color.Color, isFg bool, bold bool) uint32 {
	if c == nil {
		if isFg {
			if bold {
				if t.Bold != 0 {
					return t.Bold
				}
				return t.ANSI[15]
			}
			return t.Foreground
		}
		return t.Background
	}

	// Map ANSI indexed colors (0-15) to our theme palette
	switch v := c.(type) {
	case ansi.BasicColor:
		idx := int(v)
		if bold && isFg && idx < 8 {
			idx += 8 // promote to bright variant
		}
		if idx >= 0 && idx < 16 {
			return t.ANSI[idx]
		}
	case ansi.IndexedColor:
		idx := int(v)
		if bold && isFg && idx < 8 {
			idx += 8
		}
		if idx >= 0 && idx < 16 {
			return t.ANSI[idx]
		}
		// 16-255: extended palette — fall through to raw color
	}

	return ColorToABGR(c)
}

// hexToABGR converts a hex color string like "#FF5555" to ABGR uint32.
func hexToABGR(hex string) uint32 {
	hex = strings.TrimPrefix(hex, "#")
	if len(hex) != 6 {
		return 0xFFFFFFFF
	}
	r, _ := strconv.ParseUint(hex[0:2], 16, 8)
	g, _ := strconv.ParseUint(hex[2:4], 16, 8)
	b, _ := strconv.ParseUint(hex[4:6], 16, 8)
	return 0xFF000000 | (uint32(b) << 16) | (uint32(g) << 8) | uint32(r)
}

// HexToABGR is the exported version of hexToABGR.
func HexToABGR(hex string) uint32 {
	return hexToABGR(hex)
}

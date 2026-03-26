// Package renderer handles cell grid → ImGui draw commands.
package renderer

import (
	"image/color"
	"strconv"
	"strings"
)

// Theme holds resolved colors for rendering.
type Theme struct {
	Foreground uint32
	Background uint32
	Cursor     uint32
	SelectionFg uint32
	SelectionBg uint32
	ANSI       [16]uint32
}

// DefaultTheme returns the Dracula theme.
func DefaultTheme() Theme {
	return Theme{
		Foreground:  0xFFF2F8F8, // #F8F8F2 in ABGR
		Background:  0xFF36282A, // #282A36 in ABGR (note: ABGR = 0xAABBGGRR)
		Cursor:      0xFFF2F8F8,
		SelectionFg: 0xFFF2F8F8,
		SelectionBg: 0xFF5A4744,
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
func (t *Theme) ResolveColor(c color.Color, isFg bool) uint32 {
	if c == nil {
		if isFg {
			return t.Foreground
		}
		return t.Background
	}

	// Check for indexed ANSI colors
	if ic, ok := c.(color.RGBA); ok {
		// Check if it's an exact ANSI match — not reliable, just use raw color
		_ = ic
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

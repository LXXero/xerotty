// Package renderer handles cell grid → ImGui draw commands.
package renderer

import (
	"image/color"
	"math"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Theme holds resolved colors for rendering.
type Theme struct {
	Foreground     uint32
	Background     uint32
	Bold           uint32 // explicit bold-text color override; 0 = none
	Cursor         uint32
	SelectionFg    uint32
	SelectionBg    uint32
	ScrollbarBg    uint32
	ScrollbarThumb uint32
	ScrollbarHover uint32
	// Tabs colors the tab bar. Kept apart from the terminal colors so
	// a UI theme can replace this half without touching the palette.
	Tabs TabColors
	ANSI [16]uint32
}

// TabColors holds the tab bar's colors. A zero field is unset:
// ResolveTabs fills it from the terminal colors, so a theme that sets
// only a few keys (or none) still gets a bar whose states differ.
// MatchTerminalBg in any field means "the terminal's background".
type TabColors struct {
	BarBg        uint32 // empty strip behind the tabs
	InactiveBg   uint32
	InactiveText uint32 // before dimming
	ActiveBg     uint32
	ActiveText   uint32
	ActiveMarker uint32 // line along the bottom of the current tab
	HoverBg      uint32
	// Activity colors the top bar + glow (and the optional dot) on a
	// background tab with output you haven't looked at. Alpha is
	// ignored — the glow renders its own gradient falloff.
	Activity uint32
	Bell     uint32 // dot on a background tab that rang the bell

	// InactiveTextDimmed is what background tab titles are drawn in.
	// Derived by ResolveTabs; never read from a theme file.
	InactiveTextDimmed uint32
}

// MatchTerminalBg is the stored value of a tab color written as
// "terminal". Alpha 0 with a nonzero color can't come out of
// HexToABGR (which always sets alpha 0xFF) or mean "unset" (0), so it
// is safe as a marker until ResolveTabs swaps in the real background.
const MatchTerminalBg uint32 = 0x00000001

// WCAG 2.x contrast minimums: 4.5:1 for normal text (SC 1.4.3) and
// 3:1 for non-text marks like the activity bar (SC 1.4.11).
const (
	MinTextContrast = 4.5
	MinMarkContrast = 3.0
)

// Amber activity defaults for themes that don't pick their own. Amber
// sits apart from the blues/purples most themes use for the current
// tab, so "new output" never reads as "current tab". The dark-ground
// value is classic amber-phosphor; the light-ground one is darkened to
// keep 3:1 against a pale tab.
var (
	defaultActivityDark  = hexToABGR("#FFB000")
	defaultActivityLight = hexToABGR("#A55A00")
)

// DefaultTheme returns the xerotty theme (Tango colors, white on
// black), the same values as themes/xerotty.toml, for when no theme
// file is found at all.
func DefaultTheme() Theme {
	return Theme{
		Foreground:     hexToABGR("#FFFFFF"),
		Background:     hexToABGR("#000000"),
		Bold:           hexToABGR("#FFFFFF"),
		Cursor:         hexToABGR("#FFFFFF"),
		SelectionFg:    hexToABGR("#000000"),
		SelectionBg:    hexToABGR("#D3D7CF"),
		ScrollbarBg:    hexToABGR("#000000"),
		ScrollbarThumb: hexToABGR("#555753"),
		ScrollbarHover: hexToABGR("#888A85"),
		Tabs: TabColors{
			BarBg:        hexToABGR("#141718"),
			InactiveBg:   hexToABGR("#1F2426"),
			InactiveText: hexToABGR("#EEEEEC"),
			ActiveBg:     hexToABGR("#204A87"), // Tango dark sky blue
			ActiveText:   hexToABGR("#FFFFFF"),
			ActiveMarker: hexToABGR("#729FCF"),
			HoverBg:      hexToABGR("#283033"),
			Activity:     hexToABGR("#FCAF3E"), // Tango orange
			Bell:         hexToABGR("#FF6B6B"),
		},
		ANSI: [16]uint32{
			hexToABGR("#2E3436"), // black
			hexToABGR("#CC0000"), // red
			hexToABGR("#4E9A06"), // green
			hexToABGR("#C4A000"), // yellow
			hexToABGR("#3465A4"), // blue
			hexToABGR("#75507B"), // magenta
			hexToABGR("#06989A"), // cyan
			hexToABGR("#D3D7CF"), // white
			hexToABGR("#555753"), // bright black
			hexToABGR("#EF2929"), // bright red
			hexToABGR("#8AE234"), // bright green
			hexToABGR("#FCE94F"), // bright yellow
			hexToABGR("#729FCF"), // bright blue
			hexToABGR("#AD7FA8"), // bright magenta
			hexToABGR("#34E2E2"), // bright cyan
			hexToABGR("#EEEEEC"), // bright white
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

// ParseTabColor reads a tab color value from a theme or config file:
// "#RRGGBB", or "terminal" for the terminal's background. "" stays
// unset (0).
func ParseTabColor(v string) uint32 {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "":
		return 0
	case "terminal":
		return MatchTerminalBg
	}
	return hexToABGR(v)
}

// ResolveTabs fills every unset tab color from the terminal colors,
// replaces MatchTerminalBg, and computes the dimmed background-tab
// text. Call it after all overrides are applied, since the defaults
// and "terminal" both depend on the final Background/Foreground.
// dim is the largest blend of title toward tab color (0..1); the
// result never drops below MinTextContrast unless the undimmed text
// was already below it.
func (t *Theme) ResolveTabs(dim float64) {
	bg, fg := t.Background, t.Foreground
	tc := &t.Tabs
	fill := func(c *uint32, def uint32) {
		if *c == 0 {
			*c = def
		}
		if *c == MatchTerminalBg {
			*c = bg
		}
	}
	fill(&tc.ActiveBg, Mix(bg, fg, 0.15))
	fill(&tc.InactiveBg, shadeBg(bg, fg, 0.18))
	fill(&tc.BarBg, shadeBg(bg, fg, 0.32))
	fill(&tc.HoverBg, Mix(tc.InactiveBg, fg, 0.10))
	fill(&tc.ActiveText, fg)
	fill(&tc.InactiveText, fg)
	fill(&tc.ActiveMarker, t.ANSI[4])
	act := defaultActivityDark
	if RelativeLuminance(tc.InactiveBg) > 0.4 {
		act = defaultActivityLight
	}
	fill(&tc.Activity, act)
	fill(&tc.Bell, t.ANSI[9])
	tc.InactiveTextDimmed = DimToward(tc.InactiveText, tc.InactiveBg, dim, MinTextContrast)
}

// shadeBg derives a tab-bar shade from the terminal background:
// darker, the usual dark-theme look. A background already near black
// can't get darker, which would leave tabs identical to the terminal,
// so it is lifted toward the foreground instead.
func shadeBg(bg, fg uint32, amt float64) uint32 {
	if RelativeLuminance(bg) < 0.01 {
		return Mix(bg, fg, amt*0.4)
	}
	return Mix(bg, 0xFF000000, amt)
}

// Mix blends a toward b by t (0 = a, 1 = b). Result is opaque.
func Mix(a, b uint32, t float64) uint32 {
	ch := func(sh uint) uint32 {
		x := float64((a>>sh)&0xFF)*(1-t) + float64((b>>sh)&0xFF)*t
		return uint32(math.Round(x)) << sh
	}
	return 0xFF000000 | ch(0) | ch(8) | ch(16)
}

// RelativeLuminance is the WCAG 2.x relative luminance of an ABGR color.
func RelativeLuminance(c uint32) float64 {
	lin := func(v uint32) float64 {
		x := float64(v&0xFF) / 255
		if x <= 0.03928 {
			return x / 12.92
		}
		return math.Pow((x+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(c) + 0.7152*lin(c>>8) + 0.0722*lin(c>>16)
}

// ContrastRatio is the WCAG 2.x contrast ratio between two colors (1..21).
func ContrastRatio(a, b uint32) float64 {
	la, lb := RelativeLuminance(a), RelativeLuminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// DimToward blends fg toward bg by up to max, backing off in 1% steps
// until the contrast is at least floor. If fg itself is below floor it
// is returned unchanged — dimming would only make it worse.
func DimToward(fg, bg uint32, max, floor float64) uint32 {
	if max <= 0 || ContrastRatio(fg, bg) < floor {
		return fg
	}
	if max > 1 {
		max = 1
	}
	for t := max; t > 0; t -= 0.01 {
		if c := Mix(fg, bg, t); ContrastRatio(c, bg) >= floor {
			return c
		}
	}
	return fg
}

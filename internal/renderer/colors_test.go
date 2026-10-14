package renderer

import (
	"math"
	"testing"
)

func TestContrastRatioKnownValues(t *testing.T) {
	if got := ContrastRatio(hexToABGR("#FFFFFF"), hexToABGR("#000000")); math.Abs(got-21) > 0.01 {
		t.Fatalf("white/black = %.2f, want 21", got)
	}
	if got := ContrastRatio(hexToABGR("#777777"), hexToABGR("#777777")); got != 1 {
		t.Fatalf("same color = %.2f, want 1", got)
	}
}

// Every unset tab color must come out set, and the states the user
// has to tell apart must not collapse into one color.
func TestResolveTabsFillsUnsetFromTerminal(t *testing.T) {
	th := DefaultTheme()
	th.Tabs = TabColors{}
	th.ResolveTabs(0.4)
	tc := th.Tabs
	for name, v := range map[string]uint32{
		"bar_bg": tc.BarBg, "inactive_bg": tc.InactiveBg, "inactive_text": tc.InactiveText,
		"active_bg": tc.ActiveBg, "active_text": tc.ActiveText, "active_marker": tc.ActiveMarker,
		"hover_bg": tc.HoverBg, "activity": tc.Activity, "bell": tc.Bell,
		"inactive_text_dimmed": tc.InactiveTextDimmed,
	} {
		if v == 0 || v == MatchTerminalBg {
			t.Errorf("%s left unresolved: %#x", name, v)
		}
	}
	if tc.ActiveBg == tc.InactiveBg {
		t.Error("derived active and inactive tab backgrounds are identical")
	}
	if tc.Activity != defaultActivityDark {
		t.Errorf("dark theme activity = %#x, want amber %#x", tc.Activity, defaultActivityDark)
	}
	if tc.ActiveMarker != th.ANSI[4] {
		t.Errorf("active marker = %#x, want ANSI blue", tc.ActiveMarker)
	}
}

// A pure-black terminal can't get darker: derived tabs must still
// differ from the terminal background.
func TestResolveTabsBlackBackground(t *testing.T) {
	th := DefaultTheme()
	th.Tabs = TabColors{}
	th.Background = hexToABGR("#000000")
	th.ResolveTabs(0.4)
	if th.Tabs.InactiveBg == th.Background {
		t.Error("inactive tab matches a black terminal background")
	}
}

func TestResolveTabsLightBackgroundPicksDarkAmber(t *testing.T) {
	th := DefaultTheme()
	th.Tabs = TabColors{}
	th.Background = hexToABGR("#FDF6E3")
	th.Foreground = hexToABGR("#073642")
	th.ResolveTabs(0.4)
	if th.Tabs.Activity != defaultActivityLight {
		t.Fatalf("light theme activity = %#x, want %#x", th.Tabs.Activity, defaultActivityLight)
	}
	if r := ContrastRatio(th.Tabs.Activity, th.Tabs.InactiveBg); r < MinMarkContrast {
		t.Fatalf("light amber vs inactive tab = %.2f, want >= %.1f", r, MinMarkContrast)
	}
}

func TestResolveTabsTerminalKeyword(t *testing.T) {
	th := DefaultTheme()
	th.Tabs.ActiveBg = ParseTabColor("terminal")
	th.Background = hexToABGR("#123456")
	th.ResolveTabs(0.4)
	if th.Tabs.ActiveBg != th.Background {
		t.Fatalf(`active_bg = "terminal" resolved to %#x, want terminal bg %#x`, th.Tabs.ActiveBg, th.Background)
	}
	if ParseTabColor("") != 0 {
		t.Fatal(`"" must stay unset`)
	}
}

// The dimming floor: however large dim is, background tab titles
// never drop below WCAG AA for text; and dim really does dim.
func TestDimTowardKeepsReadabilityFloor(t *testing.T) {
	fg, bg := hexToABGR("#F8F8F2"), hexToABGR("#21222C")
	for _, d := range []float64{0.2, 0.4, 0.8, 1} {
		c := DimToward(fg, bg, d, MinTextContrast)
		if r := ContrastRatio(c, bg); r < MinTextContrast {
			t.Errorf("dim %.1f: contrast %.2f below %.1f", d, r, MinTextContrast)
		}
		if c == fg {
			t.Errorf("dim %.1f left the text undimmed", d)
		}
	}
	if c := DimToward(fg, bg, 0, MinTextContrast); c != fg {
		t.Error("dim 0 changed the text")
	}
	// Already below the floor: leave it alone rather than make it worse.
	low := hexToABGR("#44475A")
	if c := DimToward(low, bg, 0.4, MinTextContrast); c != low {
		t.Error("text below the floor was dimmed further")
	}
}

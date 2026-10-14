package app

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/renderer"
)

func hexc(s string) uint32 { return renderer.HexToABGR(s) }

// The prefs dialog saved tab_bar_bg / tab_active_bg / ... for years
// and nothing read them. With tab_colors = "custom" they now reach
// the theme, and [appearance.tabs] wins over the flat keys.
func TestTabColorOverridesReachTheme(t *testing.T) {
	cfg := config.Default()
	ap := &cfg.Appearance
	ap.TabColors = "custom"
	ap.TabBarBg = "#010101"
	ap.TabActiveBg = "#020202"
	ap.TabActiveFg = "#030303"
	ap.TabInactiveBg = "#040404"
	ap.TabInactiveFg = "#050505"
	ap.Tabs.ActiveBg = "#0A0A0A" // wins over tab_active_bg
	ap.Tabs.ActiveMarker = "#0B0B0B"
	ap.Tabs.HoverBg = "terminal"
	ap.Tabs.Activity = "#0C0C0C"
	ap.Tabs.Bell = "#0D0D0D"

	th := renderer.DefaultTheme()
	applyColorOverrides(&th, &cfg)
	th.ResolveTabs(cfg.Appearance.Tabs.InactiveTextDim)
	tc := th.Tabs
	want := map[string][2]uint32{
		"bar_bg":        {tc.BarBg, hexc("#010101")},
		"active_bg":     {tc.ActiveBg, hexc("#0A0A0A")},
		"active_text":   {tc.ActiveText, hexc("#030303")},
		"inactive_bg":   {tc.InactiveBg, hexc("#040404")},
		"inactive_text": {tc.InactiveText, hexc("#050505")},
		"active_marker": {tc.ActiveMarker, hexc("#0B0B0B")},
		"hover_bg":      {tc.HoverBg, th.Background},
		"activity":      {tc.Activity, hexc("#0C0C0C")},
		"bell":          {tc.Bell, hexc("#0D0D0D")},
	}
	for k, v := range want {
		if v[0] != v[1] {
			t.Errorf("%s = %#x, want %#x", k, v[0], v[1])
		}
	}

	// tab_colors = "theme" ignores them all.
	ap.TabColors = "theme"
	th2 := renderer.DefaultTheme()
	applyColorOverrides(&th2, &cfg)
	if th2.Tabs != renderer.DefaultTheme().Tabs {
		t.Error(`tab_colors = "theme" still applied custom tab colors`)
	}
}

func TestScrollbarOverridesReachTheme(t *testing.T) {
	cfg := config.Default()
	cfg.Appearance.ScrollbarColors = "custom"
	cfg.Appearance.ScrollbarBg = "#111111"
	cfg.Appearance.ScrollbarThumb = "#222222"
	th := renderer.DefaultTheme()
	applyColorOverrides(&th, &cfg)
	if th.ScrollbarBg != hexc("#111111") || th.ScrollbarThumb != hexc("#222222") {
		t.Fatal("custom scrollbar colors not applied")
	}
}

// loadTheme honors terminal_theme over theme and ui_theme for the UI
// half, and always comes out fully resolved.
func TestLoadThemeSplit(t *testing.T) {
	dir := t.TempDir()
	td := filepath.Join(dir, "themes")
	os.MkdirAll(td, 0o755)
	write := func(n, s string) {
		if err := os.WriteFile(filepath.Join(td, n+".toml"), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("set", "[theme]\n[theme.colors]\nbackground = \"#101010\"\n[theme.ui.tabs]\nbell = \"#AA0000\"\n")
	write("term", "[theme]\n[theme.colors]\nbackground = \"#202020\"\n[theme.ui.tabs]\nbell = \"#BB0000\"\n")
	write("ui", "[theme]\n[theme.ui.tabs]\nbell = \"#00CC00\"\n")
	t.Setenv(config.EnvConfigDir, dir)

	cfg := config.Default()
	cfg.Appearance.Theme = "set"
	if th := loadTheme(&cfg); th.Background != hexc("#101010") || th.Tabs.Bell != hexc("#AA0000") {
		t.Error("theme alone should set both halves")
	}
	cfg.Appearance.TerminalTheme = "term"
	if th := loadTheme(&cfg); th.Background != hexc("#202020") || th.Tabs.Bell != hexc("#BB0000") {
		t.Error("terminal_theme should replace the set, UI following it while ui_theme is unset")
	}
	cfg.Appearance.UITheme = "ui"
	th := loadTheme(&cfg)
	if th.Background != hexc("#202020") || th.Tabs.Bell != hexc("#00CC00") {
		t.Error("ui_theme should supply the UI half only")
	}
	if th.Tabs.InactiveTextDimmed == 0 || th.Tabs.Activity == 0 {
		t.Error("loadTheme returned unresolved tab colors")
	}
	// set_theme:<name> keeps the user's ui_theme.
	if th := loadThemeNamed("set", &cfg); th.Background != hexc("#101010") || th.Tabs.Bell != hexc("#00CC00") {
		t.Error("set_theme dropped the ui_theme pick")
	}
}

func TestSplitThemeNames(t *testing.T) {
	halves := map[string][2]bool{
		"full": {true, true}, "term": {true, false}, "uionly": {false, true}, "broken": {false, false},
	}
	term, ui := splitThemeNames([]string{"broken", "full", "term", "uionly"},
		func(n string) (bool, bool) { h := halves[n]; return h[0], h[1] })
	if !reflect.DeepEqual(term, []string{"broken", "full", "term"}) {
		t.Errorf("terminal picker = %v", term)
	}
	if !reflect.DeepEqual(ui, []string{"same", "full", "uionly"}) {
		t.Errorf("UI picker = %v", ui)
	}
}

// A config written before the split (flat tab_* keys) opens in prefs
// with its values and saves them into [appearance.tabs].
func TestPrefsDialogMigratesFlatTabKeys(t *testing.T) {
	t.Setenv(config.EnvConfigDir, t.TempDir())
	cfg := config.Default()
	cfg.Appearance.TabColors = "custom"
	cfg.Appearance.TabActiveBg = "#020202"
	cfg.Appearance.TabInactiveFg = "#050505"
	cfg.Appearance.Tabs.Bell = "#0D0D0D"
	cfg.Appearance.ScrollbarThumb = "#222222"
	cfg.Appearance.UITheme = "same"

	var d configDialog
	d.loadFrom(&cfg)
	out := cfg
	d.applyTo(&out)
	ap := out.Appearance
	if ap.Tabs.ActiveBg != "#020202" || ap.Tabs.InactiveText != "#050505" || ap.Tabs.Bell != "#0D0D0D" {
		t.Errorf("tabs after save = %+v", ap.Tabs)
	}
	if ap.TabActiveBg != "" || ap.TabInactiveFg != "" {
		t.Error("flat keys not cleared after migration")
	}
	if ap.ScrollbarThumb != "#222222" {
		t.Error("prefs save wiped scrollbar_thumb")
	}
	if ap.Tabs.ActivityStyle != "bar" || ap.Tabs.InactiveTextDim != 0.4 {
		t.Errorf("activity style / dim not round-tripped: %q %v", ap.Tabs.ActivityStyle, ap.Tabs.InactiveTextDim)
	}
	if ap.UITheme != "" {
		t.Errorf(`ui_theme "same" saved as %q, want ""`, ap.UITheme)
	}
}

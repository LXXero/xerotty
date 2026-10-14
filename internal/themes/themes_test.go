package themes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/renderer"
)

// useThemeDir points the user theme dir at dir/themes for the test.
func useThemeDir(t *testing.T, files map[string]string) {
	t.Helper()
	dir := t.TempDir()
	td := filepath.Join(dir, "themes")
	if err := os.MkdirAll(td, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(td, name+".toml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(config.EnvConfigDir, dir)
}

const termA = `
[theme]
name = "A"
[theme.colors]
foreground = "#EEEEEE"
background = "#101010"
[theme.ui]
scrollbar_thumb = "#222222"
[theme.ui.tabs]
active_bg = "#303030"
activity = "#FF0000"
`

const uiOnly = `
[theme]
name = "U"
[theme.ui]
scrollbar_thumb = "#ABCDEF"
[theme.ui.tabs]
bell = "#00FF00"
`

// The original flat [theme.ui] keys must keep working.
const legacy = `
[theme]
name = "L"
[theme.colors]
background = "#202020"
[theme.ui]
tab_activity_glow = "#34E2E2"
tab_bar_bg = "#111111"
tab_active_bg = "#2E3436"
tab_active_fg = "#EEEEEC"
tab_inactive_bg = "#262B2D"
tab_inactive_fg = "#888A85"
[theme.ui.tabs]
bar_bg = "#121212"
`

func hex(s string) uint32 { return renderer.HexToABGR(s) }

func TestLoadSetSameKeepsThemeUI(t *testing.T) {
	useThemeDir(t, map[string]string{"a": termA, "u": uiOnly})
	for _, ui := range []string{"", "same", "a"} {
		th, _ := LoadSet("a", ui)
		if th.Background != hex("#101010") || th.Tabs.ActiveBg != hex("#303030") || th.Tabs.Activity != hex("#FF0000") {
			t.Errorf("ui=%q: terminal theme's own UI half not used", ui)
		}
	}
}

func TestLoadSetMixesHalves(t *testing.T) {
	useThemeDir(t, map[string]string{"a": termA, "u": uiOnly})
	th, _ := LoadSet("a", "u")
	if th.Background != hex("#101010") || th.Foreground != hex("#EEEEEE") {
		t.Error("terminal half not taken from the terminal theme")
	}
	if th.Tabs.Bell != hex("#00FF00") || th.ScrollbarThumb != hex("#ABCDEF") {
		t.Error("UI half not taken from the UI theme")
	}
	// The UI theme owns the tabs: keys it leaves out are unset (to be
	// derived), not leaked from the terminal theme.
	if th.Tabs.ActiveBg != 0 || th.Tabs.Activity != 0 {
		t.Errorf("terminal theme tab colors leaked into a UI theme: active_bg=%#x activity=%#x",
			th.Tabs.ActiveBg, th.Tabs.Activity)
	}
}

func TestLoadSetMissingUIThemeKeepsTerminalUI(t *testing.T) {
	useThemeDir(t, map[string]string{"a": termA})
	th, _ := LoadSet("a", "does-not-exist")
	if th.Tabs.ActiveBg != hex("#303030") {
		t.Error("missing UI theme dropped the terminal theme's UI half")
	}
}

func TestLegacyFlatTabKeysLoad(t *testing.T) {
	useThemeDir(t, map[string]string{"l": legacy})
	th, _ := Load("l")
	tc := th.Tabs
	if tc.Activity != hex("#34E2E2") || tc.ActiveBg != hex("#2E3436") || tc.ActiveText != hex("#EEEEEC") ||
		tc.InactiveBg != hex("#262B2D") || tc.InactiveText != hex("#888A85") {
		t.Errorf("flat tab_* keys not read: %+v", tc)
	}
	if tc.BarBg != hex("#121212") {
		t.Error("[theme.ui.tabs] should win over flat tab_bar_bg")
	}
}

// A theme file with no tab colors derives them from its own terminal
// colors, not from the built-in ones.
func TestThemeWithoutTabsDoesNotInheritDefault(t *testing.T) {
	useThemeDir(t, map[string]string{"bare": "[theme]\n[theme.colors]\nbackground = \"#FDF6E3\"\n"})
	th, _ := Load("bare")
	if th.Tabs != (renderer.TabColors{}) {
		t.Fatalf("bare theme inherited tab colors: %+v", th.Tabs)
	}
}

func TestHalves(t *testing.T) {
	useThemeDir(t, map[string]string{"a": termA, "u": uiOnly})
	if term, ui := Halves("a"); !term || !ui {
		t.Errorf("full theme: terminal=%v ui=%v", term, ui)
	}
	if term, ui := Halves("u"); term || !ui {
		t.Errorf("UI-only theme: terminal=%v ui=%v", term, ui)
	}
	if term, ui := Halves("nope"); term || ui {
		t.Error("missing theme reported halves")
	}
}

// bundledThemes points the theme search at the repo's themes/ dir.
func bundledThemes(t *testing.T) []string {
	t.Helper()
	repo, err := filepath.Abs("../../themes")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Symlink(repo, filepath.Join(dir, "themes")); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.EnvConfigDir, dir)
	entries, err := os.ReadDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".toml") {
			names = append(names, strings.TrimSuffix(e.Name(), ".toml"))
		}
	}
	return names
}

// Every bundled theme's tab bar must be readable: titles at least
// WCAG 4.5:1 (after the default 40% dimming), activity / bell /
// current-tab line at least 3:1 against the tab they sit on, and
// every key set explicitly so the bundled look never depends on the
// derivation rules.
func TestBundledThemesTabContrast(t *testing.T) {
	names := bundledThemes(t)
	if len(names) < 15 {
		t.Fatalf("found %d bundled themes, want 12 full + 3 UI-only", len(names))
	}
	var full, uiOnly int
	for _, n := range names {
		term, ui := Halves(n)
		switch {
		case term && ui:
			full++
		case ui:
			uiOnly++
		}
		if !ui {
			t.Errorf("%s: no UI half", n)
			continue
		}
		th, _ := LoadSet("dracula", n)
		raw := th.Tabs
		th.ResolveTabs(0.4)
		tc := th.Tabs
		for key, v := range map[string]uint32{
			"bar_bg": raw.BarBg, "inactive_bg": raw.InactiveBg, "inactive_text": raw.InactiveText,
			"active_bg": raw.ActiveBg, "active_text": raw.ActiveText, "active_marker": raw.ActiveMarker,
			"hover_bg": raw.HoverBg, "activity": raw.Activity, "bell": raw.Bell,
		} {
			if v == 0 {
				t.Errorf("%s: tabs.%s not set", n, key)
			}
		}
		checks := []struct {
			what   string
			fg, bg uint32
			min    float64
		}{
			{"dimmed inactive text / inactive bg", tc.InactiveTextDimmed, tc.InactiveBg, renderer.MinTextContrast},
			{"active text / active bg", tc.ActiveText, tc.ActiveBg, renderer.MinTextContrast},
			{"inactive text / hover bg", tc.InactiveText, tc.HoverBg, renderer.MinTextContrast},
			{"activity / inactive bg", tc.Activity, tc.InactiveBg, renderer.MinMarkContrast},
			{"bell / inactive bg", tc.Bell, tc.InactiveBg, renderer.MinMarkContrast},
			{"active marker / active bg", tc.ActiveMarker, tc.ActiveBg, renderer.MinMarkContrast},
		}
		for _, c := range checks {
			if r := renderer.ContrastRatio(c.fg, c.bg); r < c.min {
				t.Errorf("%s: %s = %.2f, want >= %.1f", n, c.what, r, c.min)
			}
		}
	}
	if full < 12 || uiOnly < 3 {
		t.Errorf("bundled: %d full + %d UI-only, want >= 12 + 3", full, uiOnly)
	}
}

// renderer.DefaultTheme is the no-file fallback and claims to match
// themes/xerotty.toml, the default theme.
func TestDefaultThemeMatchesBundledXerotty(t *testing.T) {
	bundledThemes(t)
	th, _ := Load("xerotty")
	if th != renderer.DefaultTheme() {
		t.Fatalf("xerotty.toml %+v != DefaultTheme %+v", th, renderer.DefaultTheme())
	}
}

// Only tango.toml and xerotty.toml set bold; the built-in bold color must not leak
// into themes that leave it out.
func TestThemeWithoutBoldDoesNotInheritDefault(t *testing.T) {
	bundledThemes(t)
	th, _ := Load("dracula")
	if th.Bold != 0 {
		t.Fatalf("dracula inherited bold %#x", th.Bold)
	}
}

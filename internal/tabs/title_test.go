package tabs

import (
	"testing"

	"github.com/LXXero/xerotty/internal/config"
)

func TestIsShellProcess(t *testing.T) {
	shells := []string{"zsh", "bash", "-zsh", "-bash", "fish", "sh", "dash", "nu"}
	for _, s := range shells {
		if !isShellProcess(s) {
			t.Errorf("isShellProcess(%q) = false, want true", s)
		}
	}
	apps := []string{"vim", "nvim", "less", "top", "claude", "node", "python", ""}
	for _, a := range apps {
		if isShellProcess(a) {
			t.Errorf("isShellProcess(%q) = true, want false", a)
		}
	}
}

// Label is what list_tabs reports as the text on the user's tab bar.
func TestLabel(t *testing.T) {
	cases := []struct {
		desc, host, name, title, fg, want string
	}{
		{"named tab: the assigned name wins", "", "build", "zsh: ~/git", "zsh", "build"},
		{"unnamed tab with a program title", "", "", "vim notes.md", "vim", "vim notes.md"},
		{"Claude title: status glyph removed", "", "", "✳ Claude Code", "claude", "Claude Code"},
		{"Claude spinner frame: same label", "", "", "⠐ Claude Code", "claude", "Claude Code"},
		{"half-circle glyph removed", "", "", "◐ working", "node", "working"},
		{"glyph-only title falls back to the process", "", "", "✳", "claude", "claude"},
		{"no title: foreground process", "", "", "", "top", "top"},
		{"nothing known: shell", "", "", "", "", "shell"},
		{"remote tab gets the host prefix", "kh", "", "✳ Claude Code", "claude", "kh: Claude Code"},
		{"remote named tab", "kh", "logs", "tail", "tail", "kh: logs"},
	}
	for _, c := range cases {
		if got := Label(c.host, c.name, c.title, c.fg); got != c.want {
			t.Errorf("%s: Label(%q, %q, %q, %q) = %q, want %q", c.desc, c.host, c.name, c.title, c.fg, got, c.want)
		}
	}
}

// The label must match what the tab bar draws for the same tab, apart
// from the glyph removal.
func TestLabelMatchesDisplayTitle(t *testing.T) {
	tab := &Tab{Host: "kh"}
	tab.SetTitle("vim notes.md")
	if got, want := Label(tab.Host, "", tab.Title(), ""), tab.DisplayTitle(); got != want {
		t.Errorf("Label = %q, DisplayTitle = %q", got, want)
	}
	tab.SetName("build")
	if got, want := Label(tab.Host, tab.Name(), tab.Title(), ""), tab.DisplayTitle(); got != want {
		t.Errorf("named: Label = %q, DisplayTitle = %q", got, want)
	}
}

func TestStatusGlyphs(t *testing.T) {
	cases := map[string]string{
		"✳ Claude Code":     "✳",
		"◐ fixing-xyphia":   "◐",
		"⠐ Claude Code":     "⠐",
		"vim notes.md":      "",
		"A Running Process": "",
		"1 job running":     "",
		"- dash list":       "",
		"🚀 deploy":          "🚀",
		"✳":                 "✳",
		"":                  "",
	}
	for in, want := range cases {
		if got := StatusGlyphs(in); got != want {
			t.Errorf("StatusGlyphs(%q) = %q, want %q", in, got, want)
		}
	}
}

// A named tab shows the title's status glyph in front of the name only
// when the option is on; the name itself never changes.
func TestNamedTabStatusGlyph(t *testing.T) {
	cfg := &config.Config{}
	tab := &Tab{cfg: cfg}
	tab.SetName("build")
	tab.SetTitle("◐ some task")

	if got := tab.DisplayTitle(); got != "build" {
		t.Errorf("option off: DisplayTitle = %q, want %q", got, "build")
	}
	cfg.Tabs.StatusGlyphOnNamedTabs = true
	if got := tab.DisplayTitle(); got != "◐ build" {
		t.Errorf("option on: DisplayTitle = %q, want %q", got, "◐ build")
	}
	tab.SetTitle("zsh: ~/git")
	if got := tab.DisplayTitle(); got != "build" {
		t.Errorf("title without glyph: DisplayTitle = %q, want %q", got, "build")
	}
	tab.Host = "kh"
	tab.SetTitle("✳ done")
	if got := tab.DisplayTitle(); got != "kh: ✳ build" {
		t.Errorf("remote: DisplayTitle = %q, want %q", got, "kh: ✳ build")
	}
}

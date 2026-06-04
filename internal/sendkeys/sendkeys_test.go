package sendkeys

import (
	"strings"
	"testing"
)

func TestTranslate(t *testing.T) {
	cases := []struct {
		token     string
		appCursor bool
		want      string
	}{
		// The whole reason this package exists.
		{"enter", false, "\r"},
		{"Enter", false, "\r"},
		{"return", false, "\r"},

		{"esc", false, "\x1b"},
		{"tab", false, "\t"},
		{"backspace", false, "\x7f"},
		{"space", false, " "},

		// Ctrl chords — classic control bytes.
		{"ctrl+c", false, "\x03"},
		{"Ctrl+C", false, "\x03"},
		{"control+c", false, "\x03"},
		{"C-c", false, "\x03"},
		{"ctrl+[", false, "\x1b"},
		{"ctrl+@", false, "\x00"},
		{"ctrl+?", false, "\x7f"},

		// The user's case: '+' as the key, both joiners.
		{"ctrl++", false, "\x1b[43;5u"},
		{"ctrl-+", false, "\x1b[43;5u"},
		{"ctrl+-", false, "\x1b[45;5u"},
		{"ctrl--", false, "\x1b[45;5u"},

		// Alt = ESC prefix; stacking; tmux aliases.
		{"alt+x", false, "\x1bx"},
		{"M-x", false, "\x1bx"},
		{"meta+x", false, "\x1bx"},
		{"ctrl+alt+x", false, "\x1b\x18"},
		{"C-M-x", false, "\x1b\x18"},
		{"alt+enter", false, "\x1b\r"},

		// Shift on letters uppercases; on tab it's back-tab.
		{"shift+a", false, "A"},
		{"S-a", false, "A"},
		{"shift+tab", false, "\x1b[Z"},

		// Arrows: plain follows DECCKM, modified always CSI 1;m.
		{"up", false, "\x1b[A"},
		{"up", true, "\x1bOA"},
		{"home", true, "\x1bOH"},
		{"ctrl+up", false, "\x1b[1;5A"},
		{"ctrl+up", true, "\x1b[1;5A"},
		{"shift+up", false, "\x1b[1;2A"},
		{"ctrl+shift+up", false, "\x1b[1;6A"},
		{"alt+left", false, "\x1b[1;3D"},

		// Tilde keys + function keys.
		{"pageup", false, "\x1b[5~"},
		{"ctrl+pagedown", false, "\x1b[6;5~"},
		{"delete", false, "\x1b[3~"},
		{"f1", false, "\x1bOP"},
		{"f12", false, "\x1b[24~"},
		{"ctrl+f5", false, "\x1b[15;5~"},

		// No classic encoding → CSI-u.
		{"ctrl+enter", false, "\x1b[13;5u"},
		{"ctrl+1", false, "\x1b[49;5u"},
		{"ctrl+space", false, "\x00"},

		// Plain literal char tokens pass through.
		{"a", false, "a"},
		{"+", false, "+"},
		{"宽", false, "宽"},
	}
	for _, tc := range cases {
		got, err := Translate([]string{tc.token}, tc.appCursor)
		if err != nil {
			t.Errorf("%q (app=%v): %v", tc.token, tc.appCursor, err)
			continue
		}
		if string(got) != tc.want {
			t.Errorf("%q (app=%v) = %q, want %q", tc.token, tc.appCursor, got, tc.want)
		}
	}
}

func TestTranslateSequence(t *testing.T) {
	got, err := Translate([]string{"up", "up", "enter"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "\x1b[A\x1b[A\r" {
		t.Fatalf("sequence = %q", got)
	}
}

func TestTranslateErrors(t *testing.T) {
	for _, bad := range []string{"", "bogus", "ctrl+bogus", "entr"} {
		_, err := Translate([]string{bad}, false)
		if err == nil {
			t.Errorf("%q: expected error", bad)
			continue
		}
		if bad != "" && !strings.Contains(err.Error(), "enter") {
			t.Errorf("%q: error should list the vocabulary: %v", bad, err)
		}
	}
}

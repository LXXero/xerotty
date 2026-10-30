package terminal

import (
	"strings"
	"testing"

	vt "github.com/charmbracelet/x/vt"
)

// linkAfterPTY runs chunks through the same path PTY output takes and
// returns the emulator it landed in.
func linkAfterPTY(t *testing.T, chunks ...string) *vt.SafeEmulator {
	t.Helper()
	term := &Terminal{Emu: vt.NewSafeEmulator(80, 4)}
	var buf []byte
	var inOSC bool
	for _, chunk := range chunks {
		var out []byte
		out, buf, inOSC = term.preprocessOSC([]byte(chunk), buf, inOSC)
		term.Emu.Write(out)
	}
	return term.Emu
}

// TestOSC8ReachesCells: preprocessOSC strips OSC sequences from what vt
// sees, which made every hyperlink in a real tab disappear, so Cell.Link
// was never set.
func TestOSC8ReachesCells(t *testing.T) {
	const url = "https://example.com/a"
	for name, seq := range map[string]string{
		"ST":     "\x1b]8;;" + url + "\x1b\\link\x1b]8;;\x1b\\ text",
		"BEL":    "\x1b]8;;" + url + "\x07link\x1b]8;;\x07 text",
		"params": "\x1b]8;id=1:x=y;" + url + "\x1b\\link\x1b]8;;\x1b\\ text",
	} {
		emu := linkAfterPTY(t, seq)
		if got := emu.CellAt(0, 0).Link.URL; got != url {
			t.Errorf("%s: link = %q", name, got)
		}
		if got := emu.CellAt(0, 0).Content; got != "l" {
			t.Errorf("%s: sequence leaked onto the screen: cell 0 = %q", name, got)
		}
		if got := emu.CellAt(4, 0).Link.URL; got != "" {
			t.Errorf("%s: link ran past its end: %q", name, got)
		}
	}
}

// TestOSC8SplitAcrossReads: a sequence can straddle two PTY reads, and
// the link has to start between the text around it.
func TestOSC8SplitAcrossReads(t *testing.T) {
	emu := linkAfterPTY(t, "ab\x1b]8;;https://exam", "ple.com/\x1b\\cd\x1b]8;;\x1b", "\\ef")
	want := []string{"", "", "https://example.com/", "https://example.com/", "", ""}
	for col, url := range want {
		if got := emu.CellAt(col, 0).Link.URL; got != url {
			t.Errorf("col %d: link = %q, want %q", col, got, url)
		}
	}
}

// TestOSC8ParserTraps: bytes vt can't take inside the sequence are
// encoded out of the URL. 0x9c ends the string in vt's parser, and an
// extra ';' makes it ignore the sequence.
func TestOSC8ParserTraps(t *testing.T) {
	for name, c := range map[string]struct{ in, want string }{
		"utf-8 with byte 0x9c": {"https://example.com/✜", "https://example.com/%E2%9C%9C"},
		"semicolon":            {"https://example.com/a;b=c", "https://example.com/a%3Bb=c"},
		"space":                {"https://example.com/a b", "https://example.com/a%20b"},
	} {
		emu := linkAfterPTY(t, "\x1b]8;;"+c.in+"\x1b\\x\x1b]8;;\x1b\\y")
		if got := emu.CellAt(0, 0).Link.URL; got != c.want {
			t.Errorf("%s: link = %q, want %q", name, got, c.want)
		}
		if got := emu.CellAt(0, 0).Content; got != "x" {
			t.Errorf("%s: cell 0 = %q", name, got)
		}
		if got := emu.CellAt(1, 0).Content; got != "y" {
			t.Errorf("%s: cell 1 = %q", name, got)
		}
	}
}

// TestOSC8TooLongEndsLink: an over-long target isn't kept, and the text
// after it mustn't inherit the link before it.
func TestOSC8TooLongEndsLink(t *testing.T) {
	long := "https://example.com/" + strings.Repeat("a", maxLinkURL)
	emu := linkAfterPTY(t, "\x1b]8;;https://example.com/\x1b\\a\x1b]8;;"+long+"\x1b\\b\x1b]8;;\x1b\\")
	if got := emu.CellAt(0, 0).Link.URL; got != "https://example.com/" {
		t.Fatalf("link = %q", got)
	}
	if got := emu.CellAt(1, 0).Link.URL; got != "" {
		t.Fatalf("over-long link kept: %d bytes", len(got))
	}
	if got := emu.CellAt(1, 0).Content; got != "b" {
		t.Fatalf("cell 1 = %q", got)
	}
}

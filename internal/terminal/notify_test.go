package terminal

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestParseOSC9(t *testing.T) {
	cases := []struct {
		in   string
		ok   bool
		body string
	}{
		{"Build finished", true, "Build finished"},
		{"needs input; please look", true, "needs input; please look"},
		{"", false, ""},
		// ConEmu subcommands: progress bar, sleep, message box, ...
		{"4;1;50", false, ""},
		{"4;0", false, ""},
		{"1", false, ""},
		{"12", false, ""},
		{"3 tests failed", true, "3 tests failed"},
	}
	for _, c := range cases {
		n, ok := parseOSC9([]byte(c.in))
		if ok != c.ok || n.Body != c.body || (ok && (n.Protocol != "osc9" || n.Title != "")) {
			t.Errorf("parseOSC9(%q) = %+v, %v; want body %q, ok %v", c.in, n, ok, c.body, c.ok)
		}
	}
}

func TestParseOSC777(t *testing.T) {
	cases := []struct {
		in          string
		ok          bool
		title, body string
	}{
		{"notify;Claude Code;Task done", true, "Claude Code", "Task done"},
		{"notify;Title;body; with; semicolons", true, "Title", "body; with; semicolons"},
		{"notify;Only title", true, "Only title", ""},
		{"notify;;", false, "", ""},
		{"preexec", false, "", ""},
		{"precmd;x", false, "", ""},
	}
	for _, c := range cases {
		n, ok := parseOSC777([]byte(c.in))
		if ok != c.ok || n.Title != c.title || n.Body != c.body || (ok && n.Protocol != "osc777") {
			t.Errorf("parseOSC777(%q) = %+v, %v; want %q/%q ok %v", c.in, n, ok, c.title, c.body, c.ok)
		}
	}
}

func TestKittyOSC99(t *testing.T) {
	var k kittyNotifyAssembler

	// Single chunk, default payload type is the title.
	n, ok := k.add([]byte(";Hello"))
	if !ok || n.Title != "Hello" || n.Body != "" || n.Protocol != "osc99" {
		t.Fatalf("single chunk = %+v, %v", n, ok)
	}

	// Title then body, joined by id, finished by d=1 (the default).
	if _, ok := k.add([]byte("i=1:d=0;Build")); ok {
		t.Fatal("d=0 chunk must not complete")
	}
	if _, ok := k.add([]byte("i=1:d=0:p=title; done")); ok {
		t.Fatal("d=0 chunk must not complete")
	}
	n, ok = k.add([]byte("i=1:p=body;all green"))
	if !ok || n.Title != "Build done" || n.Body != "all green" {
		t.Fatalf("chunked = %+v, %v", n, ok)
	}
	if len(k.pending) != 0 || len(k.order) != 0 {
		t.Errorf("finished id left pending: %v %v", k.pending, k.order)
	}

	// Base64 payloads (e=1).
	b64 := base64.StdEncoding.EncodeToString([]byte("✳ ready"))
	n, ok = k.add([]byte("i=2:e=1;" + b64))
	if !ok || n.Title != "✳ ready" {
		t.Fatalf("base64 = %+v, %v", n, ok)
	}
	if _, ok := k.add([]byte("e=1;not base64!")); ok {
		t.Error("bad base64 must be dropped")
	}

	// Chunks with no text: capability query, close, icon.
	for _, in := range []string{"i=3:p=?;", "i=3:p=close;", "p=icon;AAAA", "no-semicolon"} {
		if n, ok := k.add([]byte(in)); ok {
			t.Errorf("%q produced %+v", in, n)
		}
	}

	// Unfinished notifications are bounded.
	for i := 0; i < kittyPendingMax+5; i++ {
		k.add([]byte("i=p" + strings.Repeat("x", i) + ":d=0;t"))
	}
	if len(k.pending) != kittyPendingMax || len(k.order) != kittyPendingMax {
		t.Errorf("pending = %d/%d, want %d", len(k.pending), len(k.order), kittyPendingMax)
	}
}

func TestClampNotifyField(t *testing.T) {
	long := strings.Repeat("a", notifyFieldMax-1) + "✳✳"
	got := clampNotifyField(long)
	if len(got) > notifyFieldMax || !strings.HasSuffix(got, "a") {
		t.Errorf("clamp split a rune: len %d, tail %q", len(got), got[len(got)-3:])
	}
	if clampNotifyField("short") != "short" {
		t.Error("short text changed")
	}
}

// The notification OSCs reach OnNotify through the same preprocessOSC
// path the PTY reader uses, and are stripped from what vt sees.
func TestPreprocessOSCNotifications(t *testing.T) {
	term := &Terminal{}
	var got []Notification
	term.SetOnNotify(func(n Notification) { got = append(got, n) })

	in := "a\x1b]9;Build finished\x07b" +
		"\x1b]777;notify;Codex;Turn complete\x1b\\c" +
		"\x1b]99;i=7:d=0;Tests\x07\x1b]99;i=7:p=body;3 failed\x1b\\" +
		"\x1b]9;4;1;50\x07d"
	// Split mid-sequence to cover state carried across reads.
	split := strings.Index(in, "Turn")
	out1, buf, inOSC := term.preprocessOSC([]byte(in[:split]), nil, false)
	out2, _, _ := term.preprocessOSC([]byte(in[split:]), buf, inOSC)
	if vis := string(out1) + string(out2); vis != "abcd" {
		t.Errorf("visible bytes = %q, want %q", vis, "abcd")
	}
	want := []Notification{
		{Protocol: "osc9", Body: "Build finished"},
		{Protocol: "osc777", Title: "Codex", Body: "Turn complete"},
		{Protocol: "osc99", Title: "Tests", Body: "3 failed"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d notifications %+v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("notification %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

package app

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
	"github.com/LXXero/xerotty/internal/daemonsource"
	"github.com/LXXero/xerotty/internal/testutil"
	vt "github.com/charmbracelet/x/vt"
)

// feed builds a real emulator and writes raw bytes to it, so these
// tests see the cells the parser really produces.
func feed(t *testing.T, cols, rows int, data string) *vt.SafeEmulator {
	t.Helper()
	emu := vt.NewSafeEmulator(cols, rows)
	if _, err := emu.Write([]byte(data)); err != nil {
		t.Fatalf("write: %v", err)
	}
	return emu
}

func osc8(target, text string) string {
	return "\x1b]8;;" + target + "\x1b\\" + text + "\x1b]8;;\x1b\\"
}

func wantSpans(t *testing.T, hit *linkHit, want ...linkSpan) {
	t.Helper()
	if fmt.Sprint(hit.Spans) != fmt.Sprint(want) {
		t.Fatalf("spans = %v, want %v", hit.Spans, want)
	}
}

// longURL is exactly n characters long.
func longURL(n int) string {
	u := "https://example.com/"
	return u + strings.Repeat("a", n-len(u))
}

// TestDetectLinkInScrollback: link hit-testing must work on rows
// scrolled up into history. Regression: detectLinkAt read the raw
// shadow emulator, whose ScrollbackLen is 0 for daemon tabs (the
// Source mirrors scrollback in its own ring) — scrolled-up rows
// computed negative content indices and links died above the live
// screen.
func TestDetectLinkInScrollback(t *testing.T) {
	g := &fakeGrid{
		sb: []string{
			"see https://example.com/x for details",
			"another history line",
		},
		screen: []string{"prompt$ ", "", ""},
		cols:   45,
	}

	// Scrolled up 2: viewport row 0 = scrollback row 0 (the URL line).
	hit := detectLinkAt(g, 10, 0, 2)
	if hit == nil {
		t.Fatal("no link found in scrolled-back row")
	}
	if hit.URL != "https://example.com/x" {
		t.Fatalf("URL = %q", hit.URL)
	}

	// Same viewport coords, not scrolled: live screen row 0 has no URL.
	if hit := detectLinkAt(g, 10, 0, 0); hit != nil {
		t.Fatalf("unexpected link on live row: %q", hit.URL)
	}
}

// TestWrappedURLClickedOnEachRow: a URL wrapped by the terminal over 5
// rows is found whole from any row, not only the first, with an
// underline span per row.
func TestWrappedURLClickedOnEachRow(t *testing.T) {
	url := longURL(90)
	emu := feed(t, 20, 8, url+"\r\nnext line\r\n")

	want := []linkSpan{{0, 0, 19}, {1, 0, 19}, {2, 0, 19}, {3, 0, 19}, {4, 0, 9}}
	for row := 0; row < 5; row++ {
		for _, col := range []int{0, 7} {
			hit := detectLinkAt(emu, col, row, 0)
			if hit == nil {
				t.Fatalf("no link at row %d col %d", row, col)
			}
			if hit.URL != url {
				t.Fatalf("row %d: URL = %q", row, hit.URL)
			}
			if hit.OSC8 {
				t.Fatal("plain text URL marked OSC 8")
			}
			wantSpans(t, hit, want...)
		}
	}
	if hit := detectLinkAt(emu, 15, 4, 0); hit != nil {
		t.Fatalf("blank past the URL's end hit %q", hit.URL)
	}
	if hit := detectLinkAt(emu, 2, 5, 0); hit != nil {
		t.Fatalf("the row after the URL hit %q", hit.URL)
	}
}

// TestWrappedURLStopsAtFirstRowThatDoesNotContinue: text on the row
// after a URL's short last row stays out of it, and so does the text
// before the URL on its first row.
func TestWrappedURLStopsAtFirstRowThatDoesNotContinue(t *testing.T) {
	url := longURL(30)
	emu := feed(t, 20, 6, "go: "+url+" then more text that keeps going\r\nunrelated\r\n")

	hit := detectLinkAt(emu, 2, 1, 0)
	if hit == nil || hit.URL != url {
		t.Fatalf("hit = %+v", hit)
	}
	wantSpans(t, hit, linkSpan{0, 4, 19}, linkSpan{1, 0, 13})
	if hit := detectLinkAt(emu, 16, 1, 0); hit != nil {
		t.Fatalf("text after the URL hit %q", hit.URL)
	}
	if hit := detectLinkAt(emu, 0, 0, 0); hit != nil {
		t.Fatalf("text before the URL hit %q", hit.URL)
	}
}

// TestURLsOnAdjacentRowsStaySeparate: a URL that ends exactly at the
// last column followed by another URL is two links.
func TestURLsOnAdjacentRowsStaySeparate(t *testing.T) {
	first, second := longURL(20), "https://other.example/"
	emu := feed(t, 20, 4, first+second+"\r\n")

	if hit := detectLinkAt(emu, 3, 0, 0); hit == nil || hit.URL != first {
		t.Fatalf("first: %+v", hit)
	}
	if hit := detectLinkAt(emu, 3, 1, 0); hit == nil || hit.URL != second {
		t.Fatalf("second: %+v", hit)
	}
}

// TestURLWrappedInsideTUIMargin is the layout in xero's screenshot: a
// TUI wraps the URL itself and pads both sides by one column, with
// rules above and below that must not join it.
func TestURLWrappedInsideTUIMargin(t *testing.T) {
	url := longURL(40)
	var b strings.Builder
	b.WriteString(" " + strings.Repeat("─", 18) + " \r\n")
	for rest := url; rest != ""; {
		n := min(18, len(rest))
		b.WriteString(" " + rest[:n] + "\r\n")
		rest = rest[n:]
	}
	b.WriteString(" " + strings.Repeat("─", 18) + " \r\n")
	emu := feed(t, 20, 8, b.String())

	for row := 1; row <= 3; row++ {
		hit := detectLinkAt(emu, 2, row, 0)
		if hit == nil || hit.URL != url {
			t.Fatalf("row %d: hit = %+v", row, hit)
		}
		wantSpans(t, hit, linkSpan{1, 1, 18}, linkSpan{2, 1, 18}, linkSpan{3, 1, 4})
	}
	if hit := detectLinkAt(emu, 5, 0, 0); hit != nil {
		t.Fatalf("rule hit %q", hit.URL)
	}
}

// TestURLContinuationNeedsSameMargin: a continuation indented by a
// different amount than the first row is separate text.
func TestURLContinuationNeedsSameMargin(t *testing.T) {
	url := "https://example.c/a"
	emu := feed(t, 20, 4, " "+url+"\r\n   more\r\n")

	hit := detectLinkAt(emu, 5, 0, 0)
	if hit == nil || hit.URL != url {
		t.Fatalf("hit = %+v", hit)
	}
}

// TestOSC8LinkWithoutURLText is the "Click here to authenticate" case:
// the visible text holds no URL, so only Cell.Link can find it.
func TestOSC8LinkWithoutURLText(t *testing.T) {
	url := "https://accounts.example.com/o/oauth2/auth?a=1&b=2"
	emu := feed(t, 80, 4, "→ "+osc8(url, "Click here to authenticate")+" or not\r\n")

	for _, col := range []int{2, 7, 27} {
		hit := detectLinkAt(emu, col, 0, 0)
		if hit == nil {
			t.Fatalf("no link at col %d", col)
		}
		if hit.URL != url || !hit.OSC8 {
			t.Fatalf("hit = %+v", hit)
		}
		wantSpans(t, hit, linkSpan{0, 2, 27})
	}
	for _, col := range []int{0, 1, 28, 30} {
		if hit := detectLinkAt(emu, col, 0, 0); hit != nil {
			t.Fatalf("col %d outside the link hit %q", col, hit.URL)
		}
	}
}

// TestOSC8LinkAcrossRows covers both ways an OSC 8 link spans rows: the
// terminal wrapping it, and a TUI printing each row inside a margin.
func TestOSC8LinkAcrossRows(t *testing.T) {
	url := "https://example.com/target"

	t.Run("terminal wrap", func(t *testing.T) {
		emu := feed(t, 10, 5, "ab"+osc8(url, strings.Repeat("x", 25))+"\r\nafter\r\n")
		for row := 0; row < 3; row++ {
			hit := detectLinkAt(emu, 3, row, 0)
			if hit == nil || hit.URL != url {
				t.Fatalf("row %d: hit = %+v", row, hit)
			}
			wantSpans(t, hit, linkSpan{0, 2, 9}, linkSpan{1, 0, 9}, linkSpan{2, 0, 6})
		}
		if hit := detectLinkAt(emu, 1, 0, 0); hit != nil {
			t.Fatalf("text before the link hit %q", hit.URL)
		}
	})

	t.Run("margin", func(t *testing.T) {
		emu := feed(t, 12, 5, " "+osc8(url, strings.Repeat("x", 10))+"\r\n "+osc8(url, "xxx")+"\r\n")
		hit := detectLinkAt(emu, 2, 1, 0)
		if hit == nil || hit.URL != url {
			t.Fatalf("hit = %+v", hit)
		}
		wantSpans(t, hit, linkSpan{0, 1, 10}, linkSpan{1, 1, 3})
	})

	t.Run("neighbouring link with another target", func(t *testing.T) {
		emu := feed(t, 10, 4, osc8(url, strings.Repeat("x", 10))+osc8(url+"/2", "yyy")+"\r\n")
		hit := detectLinkAt(emu, 1, 0, 0)
		if hit == nil || hit.URL != url {
			t.Fatalf("hit = %+v", hit)
		}
		wantSpans(t, hit, linkSpan{0, 0, 9})
	})
}

// TestOSC8RefusedSchemes: the program picks the target, so anything but
// http(s) is not a link, even under text that looks like a good URL.
func TestOSC8RefusedSchemes(t *testing.T) {
	for _, target := range []string{
		"file:///etc/passwd",
		"ssh://host/",
		"javascript:alert(1)",
		"x-custom-handler://run",
		"-oProxyCommand=evil",
		"http://",
	} {
		emu := feed(t, 60, 3, osc8(target, "https://looks-safe.example/ shown")+"\r\n")
		if hit := detectLinkAt(emu, 5, 0, 0); hit != nil {
			t.Fatalf("%q: opened %q", target, hit.URL)
		}
	}
	emu := feed(t, 60, 3, osc8("HTTPS://example.com/", "upper")+"\r\n")
	if hit := detectLinkAt(emu, 1, 0, 0); hit == nil {
		t.Fatal("an upper-case https scheme is still https")
	}
}

// TestLinkTooltipText: a target too long to show is cut, on a character
// boundary, with the start kept.
func TestLinkTooltipText(t *testing.T) {
	if got := linkTooltipText("https://example.com/"); got != "https://example.com/" {
		t.Fatalf("short URL changed: %q", got)
	}
	long := "https://example.com/" + strings.Repeat("é", maxLinkTooltip)
	got := linkTooltipText(long)
	if !strings.HasPrefix(got, "https://example.com/") || !strings.HasSuffix(got, "…") || len(got) > maxLinkTooltip+len("…") {
		t.Fatalf("got %d bytes", len(got))
	}
	if strings.ToValidUTF8(got, "") != got {
		t.Fatal("cut inside a character")
	}
}

// TestURLAfterWideCharacters: columns and byte offsets differ after
// multibyte text and wide cells. The old code compared them directly,
// which shifted the hit and cut the URL.
func TestURLAfterWideCharacters(t *testing.T) {
	url := "https://example.com/x"

	t.Run("wide", func(t *testing.T) {
		emu := feed(t, 40, 3, "日本語 "+url+"\r\n")
		hit := detectLinkAt(emu, 7, 0, 0)
		if hit == nil || hit.URL != url {
			t.Fatalf("hit = %+v", hit)
		}
		wantSpans(t, hit, linkSpan{0, 7, 27})
		if hit := detectLinkAt(emu, 3, 0, 0); hit != nil {
			t.Fatalf("wide text hit %q", hit.URL)
		}
		if hit := detectLinkAt(emu, 28, 0, 0); hit != nil {
			t.Fatalf("past the URL hit %q", hit.URL)
		}
	})

	t.Run("multibyte", func(t *testing.T) {
		emu := feed(t, 40, 3, "→→ "+url+"\r\n")
		hit := detectLinkAt(emu, 3, 0, 0)
		if hit == nil || hit.URL != url {
			t.Fatalf("hit = %+v", hit)
		}
		wantSpans(t, hit, linkSpan{0, 3, 23})
	})

	t.Run("wide in the URL", func(t *testing.T) {
		wide := "https://example.com/日本語/x"
		emu := feed(t, 60, 3, wide+"\r\n")
		// Column 21 is the right half of 日.
		hit := detectLinkAt(emu, 21, 0, 0)
		if hit == nil || hit.URL != wide {
			t.Fatalf("hit = %+v", hit)
		}
		wantSpans(t, hit, linkSpan{0, 0, 27})
	})

	t.Run("wide in an OSC 8 link", func(t *testing.T) {
		emu := feed(t, 40, 3, osc8(url, "日本語")+"\r\n")
		hit := detectLinkAt(emu, 5, 0, 0)
		if hit == nil || hit.URL != url {
			t.Fatalf("hit = %+v", hit)
		}
		wantSpans(t, hit, linkSpan{0, 0, 5})
	})
}

// TestLinksInScrollback: wrapped and OSC 8 links in history, including
// one that runs off the top of the viewport, whose spans must say so.
func TestLinksInScrollback(t *testing.T) {
	url := longURL(50)
	target := "https://example.com/osc8"
	data := url + "\r\n" + osc8(target, "history link") + "\r\n"
	for i := 0; i < 10; i++ {
		data += fmt.Sprintf("filler %d\r\n", i)
	}
	emu := feed(t, 20, 4, data)
	sb := emu.ScrollbackLen()
	if sb < 9 {
		t.Fatalf("scrollback = %d", sb)
	}

	// Viewport starting on the URL's second row, content row 1.
	off := sb - 1
	hit := detectLinkAt(emu, 3, 1, off)
	if hit == nil || hit.URL != url {
		t.Fatalf("hit = %+v", hit)
	}
	wantSpans(t, hit, linkSpan{-1, 0, 19}, linkSpan{0, 0, 19}, linkSpan{1, 0, 9})

	// The OSC 8 row is content row 3.
	hit = detectLinkAt(emu, 3, 2, off)
	if hit == nil || hit.URL != target || !hit.OSC8 {
		t.Fatalf("hit = %+v", hit)
	}
	wantSpans(t, hit, linkSpan{2, 0, 11})
}

// TestLinksInDaemonTab runs both kinds of link through a real daemon
// connection, on the live screen and in scrollback, with the daemon's
// history in memory and on disk.
func TestLinksInDaemonTab(t *testing.T) {
	const target = "https://example.com/daemon"
	url := longURL(90)

	for _, mode := range []string{"", "unlimited"} {
		t.Run("scrollback mode "+mode, func(t *testing.T) {
			sockPath := filepath.Join(testutil.SockDir(t), "xerottyd.sock")
			cfg := config.Default()
			if mode != "" {
				cfg.Scrollback.Mode = mode
			}
			d := daemon.New(&cfg, sockPath)
			doneRun := make(chan error, 1)
			go func() { doneRun <- d.Run() }()
			defer func() { _ = d.Stop(); <-doneRun }()
			time.Sleep(50 * time.Millisecond)

			cli, err := clientproto.Dial(sockPath)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer cli.Close()
			if _, err := cli.Hello("links-test"); err != nil {
				t.Fatalf("hello: %v", err)
			}
			go cli.Run()
			if err := cli.Attach("", false); err != nil {
				t.Fatalf("attach: %v", err)
			}
			<-cli.Attached()

			hub := daemonsource.NewHub(cli)
			if mode == "unlimited" {
				hub.SetScrollbackWindowed()
			}
			defer hub.Stop()

			src, err := hub.NewTab(20, 12, "")
			if err != nil {
				t.Fatalf("new tab: %v", err)
			}

			// find waits for a row of the viewport to show text.
			find := func(text string) (int, bool) {
				for row := 0; row < 12; row++ {
					if strings.Contains(extractRow(src, row, 20), text) {
						return row, true
					}
				}
				return 0, false
			}
			waitFor := func(what string, ok func() bool) {
				t.Helper()
				deadline := time.Now().Add(8 * time.Second)
				for !ok() {
					if time.Now().After(deadline) {
						t.Fatalf("timed out waiting for %s", what)
					}
					select {
					case <-src.DataChan():
					case <-time.After(50 * time.Millisecond):
					}
				}
			}

			cmd := `clear; printf '\033]8;;` + target + `\033\134LINKTEXT\033]8;;\033\134\n%s\nDONE%s\n' ` + url + ` 1` + "\r"
			if _, err := src.Write([]byte(cmd)); err != nil {
				t.Fatalf("write: %v", err)
			}
			var row int
			waitFor("the link text", func() bool {
				var ok bool
				row, ok = find("LINKTEXT")
				_, done := find("DONE1")
				return ok && done
			})

			hit := detectLinkAt(src, 2, row, 0)
			if hit == nil || hit.URL != target || !hit.OSC8 {
				t.Fatalf("live screen OSC 8: hit = %+v", hit)
			}
			wantSpans(t, hit, linkSpan{row, 0, 7})

			hit = detectLinkAt(src, 3, row+3, 0)
			if hit == nil || hit.URL != url {
				t.Fatalf("live screen wrapped URL: hit = %+v", hit)
			}

			// Push all of it into history.
			if _, err := src.Write([]byte("seq 1 40\r")); err != nil {
				t.Fatalf("write: %v", err)
			}
			waitFor("scrollback", func() bool { return src.ScrollbackLen() > 20 })
			sb := src.ScrollbackLen()

			// Find the rows in history by their text.
			linkIdx := -1
			waitFor("the link row in history", func() bool {
				sb = src.ScrollbackLen()
				src.EnsureScrollbackWindow(0, sb)
				for i := 0; i < sb; i++ {
					if strings.Contains(scrollbackRowText(src, i, 20), "LINKTEXT") {
						linkIdx = i
						return true
					}
				}
				return false
			})

			// Viewport row 0 on the link row.
			off := sb - linkIdx
			hit = detectLinkAt(src, 2, 0, off)
			if hit == nil || hit.URL != target || !hit.OSC8 {
				t.Fatalf("scrollback OSC 8 (%d rows, row %d): hit = %+v", sb, linkIdx, hit)
			}
			hit = detectLinkAt(src, 3, 3, off)
			if hit == nil || hit.URL != url {
				t.Fatalf("scrollback wrapped URL: hit = %+v", hit)
			}
		})
	}
}

func extractRow(g linkGrid, row, cols int) string {
	var b strings.Builder
	for col := 0; col < cols; col++ {
		if c := g.CellAt(col, row); c != nil {
			b.WriteString(c.Content)
		}
	}
	return b.String()
}

func scrollbackRowText(g linkGrid, row, cols int) string {
	var b strings.Builder
	for col := 0; col < cols; col++ {
		if c := g.ScrollbackCellAt(col, row); c != nil {
			b.WriteString(c.Content)
		}
	}
	return b.String()
}

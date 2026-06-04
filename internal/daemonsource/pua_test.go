package daemonsource_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
	"github.com/LXXero/xerotty/internal/daemonsource"
)

// TestPUAGlyphsSurviveWire regression-tests Private Use Area
// codepoints through the full daemon → wire → shadow-emulator path.
// Powerline / Nerd Font prompt glyphs live in the PUA — the branch
// glyph U+E0A0 in the BMP block, and most Nerd Font icons (e.g. the
// terraform workspace glyph U+F1062) in the plane-15 supplementary
// block — and a prompt like starship's emits them constantly. They
// must arrive in the GUI's shadow grid byte-identical to what the
// daemon's emulator holds, or daemon-mode tabs render missing glyphs
// that pty-mode tabs show fine.
func TestPUAGlyphsSurviveWire(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "xerottyd.sock")
	cfg := config.Default()
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
	cli.Hello("pua-test")
	go cli.Run()
	cli.Attach("", false)
	<-cli.Attached()

	hub := daemonsource.NewHub(cli)
	defer hub.Stop()
	src, err := hub.NewTab(60, 10, "")
	if err != nil {
		t.Fatalf("new tab: %v", err)
	}

	// U+E0A0 (branch), U+F1062 (terraform). END anchors the scan so
	// we know the whole echo line landed, not a partial frame.
	const bmpPUA = ""
	const supPUA = "\U000F1062"
	src.Write([]byte("echo " + bmpPUA + supPUA + " END\r"))

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if scanViewport(src, "END") {
			break
		}
		select {
		case <-src.DataChan():
		case <-time.After(80 * time.Millisecond):
		}
	}
	if !scanViewport(src, "END") {
		t.Fatal("echo output never reached the shadow viewport")
	}
	if !scanViewport(src, bmpPUA) {
		t.Errorf("BMP PUA glyph U+E0A0 lost on the wire")
	}
	if !scanViewport(src, supPUA) {
		t.Errorf("supplementary PUA glyph U+F1062 lost on the wire")
	}
}

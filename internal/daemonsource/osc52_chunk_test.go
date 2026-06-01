package daemonsource_test

import (
	"bytes"
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
	"github.com/LXXero/xerotty/internal/daemonsource"
)

// TestOSC52ClipboardSet drives an OSC 52 clipboard-set sequence
// through a daemon tab and verifies the hub's clipboard callback
// fires with the decoded text — the server→client clipboard sync
// path (a remote app copies, the local OS clipboard should get it).
func TestOSC52ClipboardSet(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "xerottyd.sock")
	cfg := config.Default()
	d := daemon.New(&cfg, sockPath)
	doneRun := make(chan error, 1)
	go func() { doneRun <- d.Run() }()
	defer func() { _ = d.Stop(); <-doneRun }()
	time.Sleep(50 * time.Millisecond)

	cli, _ := clientproto.Dial(sockPath)
	defer cli.Close()
	cli.Hello("osc52-test")
	go cli.Run()
	cli.Attach("", false)
	<-cli.Attached()

	hub := daemonsource.NewHub(cli)
	defer hub.Stop()

	got := make(chan string, 1)
	hub.SetClipboardSetCallback(func(text string) {
		select {
		case got <- text:
		default:
		}
	})

	src, err := hub.NewTab(80, 24, "")
	if err != nil {
		t.Fatalf("new tab: %v", err)
	}

	// Make the shell emit an OSC 52 set: printf the escape with a
	// base64 payload. ESC ]52;c;<b64> BEL.
	secret := "clipboard-from-remote-app"
	b64 := base64.StdEncoding.EncodeToString([]byte(secret))
	cmd := "printf '\\033]52;c;" + b64 + "\\007'\r"
	if _, err := src.Write([]byte(cmd)); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case text := <-got:
		if text != secret {
			t.Errorf("OSC52 set: got %q want %q", text, secret)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("clipboard-set callback never fired for OSC 52")
	}
}

// TestChunkedImagePaste sends an image larger than one chunk and
// confirms the daemon reassembles it + types a temp-file path the
// PTY echoes. Proves the chunked InputImage path end-to-end.
func TestChunkedImagePaste(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "xerottyd.sock")
	cfg := config.Default()
	d := daemon.New(&cfg, sockPath)
	doneRun := make(chan error, 1)
	go func() { doneRun <- d.Run() }()
	defer func() { _ = d.Stop(); <-doneRun }()
	time.Sleep(50 * time.Millisecond)

	cli, _ := clientproto.Dial(sockPath)
	defer cli.Close()
	cli.Hello("chunk-test")
	go cli.Run()
	cli.Attach("", false)
	<-cli.Attached()

	hub := daemonsource.NewHub(cli)
	defer hub.Stop()
	src, err := hub.NewTab(80, 24, "")
	if err != nil {
		t.Fatalf("new tab: %v", err)
	}
	<-src.DataChan() // first paint

	// ~2.5 MiB → 3 chunks at 1 MiB each. Content is a PNG header
	// followed by filler; daemon doesn't inspect it.
	data := bytes.Repeat([]byte{0x89, 'P', 'N', 'G', 1, 2, 3, 4}, 320*1024)
	if err := src.PasteImage("image/png", "shot", data); err != nil {
		t.Fatalf("paste image: %v", err)
	}

	// The daemon writes a temp file + types its path. The path
	// (xerotty-paste-shot-*.png) should echo into the viewport.
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if viewportHas(src, "xerotty-paste-shot-") {
			return
		}
		select {
		case <-src.DataChan():
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatal("typed temp-file path never appeared — chunked image likely didn't reassemble")
}

func viewportHas(s *daemonsource.Source, needle string) bool {
	for _, row := range s.SnapshotViewport() {
		var sb strings.Builder
		for _, c := range row {
			if c.Content == "" {
				sb.WriteByte(' ')
				continue
			}
			sb.WriteString(c.Content)
		}
		if strings.Contains(sb.String(), needle) {
			return true
		}
	}
	return false
}

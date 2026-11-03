package platform

import (
	"bytes"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestWaylandClipboardRoundTrip drives the data-control clipboard on
// a live compositor: write through the cgo path (the call that
// panicked on cgo's pointer check in 6aba4ce's parent), then read it
// back from the offer and through wl-paste. It replaces the user's
// clipboard, so it only runs when asked for:
//
//	XEROTTY_WAYLAND_CLIPBOARD_TEST=1 go test ./internal/platform/ -run RoundTrip
func TestWaylandClipboardRoundTrip(t *testing.T) {
	if os.Getenv("XEROTTY_WAYLAND_CLIPBOARD_TEST") == "" || os.Getenv("WAYLAND_DISPLAY") == "" {
		t.Skip("set XEROTTY_WAYLAND_CLIPBOARD_TEST=1 under a Wayland session (replaces the clipboard)")
	}
	if !wlclipConnectOwnDisplay() {
		t.Skip("compositor has no data-control protocol")
	}
	if !WaylandClipboardAvailable() {
		t.Fatal("wlclip_init returned 1 but WaylandClipboardAvailable is false")
	}

	want := []byte("xerotty wlclip round trip " + time.Now().Format(time.RFC3339Nano))
	if !WaylandClipboardWrite(map[string][]byte{
		"text/plain;charset=utf-8": want,
		"text/plain":               want,
		"application/x-xerotty":    {},
	}) {
		t.Fatal("WaylandClipboardWrite returned false")
	}

	// Our own source answers from the store, no dispatch needed.
	got, ok := WaylandClipboardRead("text/plain")
	if !ok || !bytes.Equal(got, want) {
		t.Fatalf("read back own source: ok=%v got=%q want=%q", ok, got, want)
	}
	if got, ok := WaylandClipboardRead("application/x-xerotty"); !ok || len(got) != 0 {
		t.Fatalf("empty entry: ok=%v len=%d, want ok with 0 bytes", ok, len(got))
	}

	// Another client must be able to paste it, which exercises the
	// send thread. wl-paste's request arrives on our display; we are
	// not dispatching here, so give it a moment then dispatch once.
	if _, err := exec.LookPath("wl-paste"); err == nil {
		done := make(chan []byte, 1)
		go func() {
			out, _ := exec.Command("timeout", "3", "wl-paste", "--no-newline", "--type", "text/plain").Output()
			done <- out
		}()
		deadline := time.Now().Add(3 * time.Second)
		for {
			select {
			case out := <-done:
				if !bytes.Equal(out, want) {
					t.Fatalf("wl-paste saw %q, want %q", out, want)
				}
				return
			default:
			}
			if time.Now().After(deadline) {
				t.Fatal("wl-paste did not return within 3s")
			}
			wlclipPumpOwnDisplay()
			time.Sleep(20 * time.Millisecond)
		}
	}
}

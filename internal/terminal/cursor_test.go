package terminal

import (
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/config"
)

// TestCursorStyleDECSCUSR drives each DECSCUSR code (0..6) through a
// real Terminal and checks the (shape, blink) the GUI would render.
// Guards two bugs the audits caught:
//   - shape enum: must be the vt enum (0=block, 1=underline, 2=bar),
//     not raw DECSCUSR.
//   - blink: x/vt hands the callback the STEADY flag (inverse of
//     blink); the handler must re-invert so blinking codes report
//     blink=true.
//
// DECSCUSR reference:
//   0 / 1 → blinking block      2 → steady block
//   3     → blinking underline  4 → steady underline
//   5     → blinking bar        6 → steady bar
func TestCursorStyleDECSCUSR(t *testing.T) {
	cases := []struct {
		code      int
		wantStyle uint8 // vt enum: 0 block, 1 underline, 2 bar
		wantBlink bool
	}{
		{1, 0, true},
		{2, 0, false},
		{3, 1, true},
		{4, 1, false},
		{5, 2, true},
		{6, 2, false},
	}

	cfg := config.Default()
	for _, tc := range cases {
		t.Run(decscusrName(tc.code), func(t *testing.T) {
			term, err := New(&cfg, 80, 24, "")
			if err != nil {
				t.Skipf("PTY unavailable: %v", err)
			}
			defer term.Close()

			// Prime with a DIFFERENT style first. vt suppresses
			// the CursorStyle callback when the requested style
			// equals the current one (no change). The emulator's
			// default is blinking block (= DECSCUSR 1), so
			// without priming, DECSCUSR 1 would be a silent
			// no-op and cursorStyleSet would never flip. Priming
			// with a guaranteed-different code makes every target
			// a real transition.
			prime := 1
			if tc.code == 1 {
				prime = 2
			}
			term.publishMu.Lock()
			term.Emu.Write([]byte("\x1b[" + itoa(prime) + " q"))
			term.Emu.Write([]byte("\x1b[" + itoa(tc.code) + " q"))
			term.publishMu.Unlock()

			// The callback fires synchronously inside Emu.Write,
			// but give a tiny beat in case of internal buffering.
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				_, _, set := term.CursorStyle()
				if set {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}

			style, blink, set := term.CursorStyle()
			if !set {
				t.Fatalf("DECSCUSR %d: cursorStyleSet never went true", tc.code)
			}
			if style != tc.wantStyle {
				t.Errorf("DECSCUSR %d: shape = %d, want %d", tc.code, style, tc.wantStyle)
			}
			if blink != tc.wantBlink {
				t.Errorf("DECSCUSR %d: blink = %v, want %v", tc.code, blink, tc.wantBlink)
			}
		})
	}
}

// itoa is defined in terminal.go (used by readPTY etc.)? Provide a
// tiny local one if not — keep the test self-contained.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func decscusrName(code int) string {
	switch code {
	case 1:
		return "blinking-block"
	case 2:
		return "steady-block"
	case 3:
		return "blinking-underline"
	case 4:
		return "steady-underline"
	case 5:
		return "blinking-bar"
	case 6:
		return "steady-bar"
	}
	return "code" + itoa(code)
}

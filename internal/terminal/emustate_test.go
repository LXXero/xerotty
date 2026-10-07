package terminal

import (
	"reflect"
	"testing"

	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/sendkeys"
	"github.com/charmbracelet/x/vt"
)

// appStateSetup is what a full-screen app might leave behind: app
// cursor keys, keypad application mode, the alt screen, left/right
// margin mode, a scroll region and left/right margins, DEC Special
// Graphics in G1, UK in G2 invoked into both GL (LS2) and GR (LS2R),
// origin mode on and autowrap off. Octal escapes so /bin/sh's printf
// takes it. No SO/SI: this vt never handles them (its SO/SI cases sit
// in the C1 loop), so GL only moves by locking shift.
const appStateSetup = `\033[?1h\033=\033[?1049h\033[?69h\033[3;20r\033[5;70s\033)0\033*A\033n\033}\033[?6h\033[?7l`

var (
	appStateMargins  = Margins{Top: 3, Bottom: 20, Left: 5, Right: 70}
	appStateCharsets = Charsets{G: [4]byte{'B', '0', 'A', 'B'}, GL: 2, GR: 2}
)

// TestEmuInternalsReadable guards the reflection in emuInternals: a
// vt bump that renames the fields it reads must fail here, not
// silently stop carrying margins and charsets across a resume.
func TestEmuInternalsReadable(t *testing.T) {
	se := vt.NewSafeEmulator(80, 24)
	m, cs, ok := emuInternals(se)
	if !ok {
		t.Fatal("emuInternals cannot read this vt version's layout")
	}
	if m != (Margins{Top: 1, Bottom: 24, Left: 1, Right: 80}) || !cs.isDefault() {
		t.Fatalf("fresh emulator: margins %+v charsets %+v", m, cs)
	}
	_, _ = se.Write([]byte("\x1b[?69h\x1b[3;20r\x1b[5;70s\x1b)0\x1b*A\x1bn\x1b}"))
	m, cs, ok = emuInternals(se)
	if !ok || m != appStateMargins || cs != appStateCharsets {
		t.Fatalf("after setup: ok %v margins %+v charsets %+v, want %+v %+v", ok, m, cs, appStateMargins, appStateCharsets)
	}
}

// TestAdoptReplaysEmuState: everything CaptureEmuState records comes
// back from Adopt identical — screen, cursor (absolute, though origin
// mode is on), modes, scroll margins, charsets — and the arrow keys of
// the adopted tab encode in application mode.
func TestAdoptReplaysEmuState(t *testing.T) {
	cfg := config.Default()
	cfg.Shell = "/bin/sh"
	old, err := NewDaemonHosted(&cfg, 80, 24, "")
	if err != nil {
		t.Skipf("no PTY available: %v", err)
	}
	// `read` parks the shell: no prompt lands after the capture.
	old.Write([]byte("printf '" + appStateSetup + "'; printf 'EMU_%s' READY; read x\r"))
	waitFor(t, old, "EMU_READY")
	es := old.CaptureEmuState()
	if es.Margins == nil || *es.Margins != appStateMargins {
		t.Fatalf("captured margins %+v, want %+v", es.Margins, appStateMargins)
	}
	if es.Charsets == nil || *es.Charsets != appStateCharsets {
		t.Fatalf("captured charsets %+v, want %+v", es.Charsets, appStateCharsets)
	}

	ptmx, pid, disk, err := old.ReleaseForHandoff()
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	retireOldReader(t, old, ptmx)
	neu, err := Adopt(AdoptSpec{
		Ptmx: ptmx, ChildPID: pid, Cols: 80, Rows: 24, Disk: disk,
		Screen: es.Screen, CursorRow: es.CursorRow, CursorCol: es.CursorCol,
		AppCursor:   es.AppCursor,
		CursorStyle: es.CursorStyle, CursorBlink: es.CursorBlink, CursorStyleSet: es.CursorStyleSet,
		DECModesSet: es.DECModesSet, DECModesReset: es.DECModesReset,
		ANSIModesSet: es.ANSIModesSet, ANSIModesReset: es.ANSIModesReset,
		Margins: es.Margins, Charsets: es.Charsets,
	})
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	defer neu.Close()

	got := neu.CaptureEmuState()
	v1, v2 := reflect.ValueOf(es), reflect.ValueOf(got)
	for i := 0; i < v1.NumField(); i++ {
		if !reflect.DeepEqual(v1.Field(i).Interface(), v2.Field(i).Interface()) {
			t.Errorf("%s: adopted %+v, captured %+v", v1.Type().Field(i).Name, v2.Field(i).Interface(), v1.Field(i).Interface())
		}
	}
	if !neu.IsAltScreen() {
		t.Error("alt screen lost across adopt")
	}
	keys, err := sendkeys.Translate([]string{"Up"}, neu.AppCursorMode())
	if err != nil {
		t.Fatal(err)
	}
	if string(keys) != "\x1bOA" {
		t.Errorf("Up encodes as %q after adopt, want ESC O A", keys)
	}
}

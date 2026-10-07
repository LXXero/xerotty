// Emulator state a resumed terminal must replay, captured in one
// piece. Both ways a daemon hands its tabs on use it: the in-place
// upgrade handoff, and the state the daemon streams to its supervisor
// for a crash resume. They used to capture different subsets, and a
// crash resume came back with app cursor keys off (mutt's arrows sent
// the wrong sequence) and no scroll region (windows scrolled whole).

package terminal

import (
	"reflect"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

// Margins is the active screen's scroll region as the DECSTBM /
// DECSLRM parameters that set it: 1-based, inclusive.
type Margins struct{ Top, Bottom, Left, Right int }

// Charsets is the ISO 2022 character set state: the SCS final byte
// designated into G0..G3 ('B' ASCII, '0' DEC Special Graphics, 'A'
// UK) and which of them GL and GR invoke.
type Charsets struct {
	G      [4]byte
	GL, GR int
}

// isDefault: ASCII everywhere and G0 in GL. GR is 0 in a fresh vt
// emulator and 1 after RIS; either way it only maps single bytes
// >= 0x80, which UTF-8 never produces alone, so both count.
func (cs Charsets) isDefault() bool {
	return cs.G == [4]byte{'B', 'B', 'B', 'B'} && cs.GL == 0 && cs.GR <= 1
}

// EmuState is everything Adopt replays into a fresh emulator. Margins
// and Charsets are nil when they hold the power-on default.
type EmuState struct {
	Screen                       [][]uv.Cell
	CursorRow, CursorCol         int
	CursorStyle                  uint8
	CursorBlink, CursorStyleSet  bool
	AppCursor                    bool
	DECModesSet, DECModesReset   []int
	ANSIModesSet, ANSIModesReset []int
	Margins                      *Margins
	Charsets                     *Charsets
}

// CaptureEmuState snapshots the emulator state under one publishMu
// hold (no PTY output lands mid-capture, so the screen, cursor and
// modes describe the same instant) and mu (no resize moves the
// margins under the read).
func (t *Terminal) CaptureEmuState() EmuState {
	t.publishMu.Lock()
	defer t.publishMu.Unlock()
	st := EmuState{Screen: t.viewportLocked()}
	pos := t.Emu.CursorPosition()
	st.CursorRow, st.CursorCol = pos.Y, pos.X
	st.CursorStyle, st.CursorBlink, st.CursorStyleSet = t.CursorStyle()
	st.AppCursor = t.AppCursorMode()
	st.DECModesSet, st.DECModesReset, st.ANSIModesSet, st.ANSIModesReset = t.ModeSnapshot()
	t.mu.Lock()
	m, cs, ok := emuInternals(t.Emu)
	t.mu.Unlock()
	if ok {
		w, h := t.Emu.Width(), t.Emu.Height()
		if m != (Margins{Top: 1, Bottom: h, Left: 1, Right: w}) {
			st.Margins = &m
		}
		if !cs.isDefault() {
			st.Charsets = &cs
		}
	}
	return st
}

// emuInternals reads the scroll region and charset registers, which
// the vt fork keeps unexported, by reflection (read-only: nothing is
// written this way; Adopt restores them with escape sequences). ok is
// false when the fork's layout no longer matches — a dependency bump
// — and then nothing is carried rather than a guess;
// TestEmuInternalsReadable fails on such a bump so it cannot land
// unnoticed. Caller holds publishMu and mu: the only writers of these
// fields are PTY ingest and Resize.
func emuInternals(se *vt.SafeEmulator) (m Margins, cs Charsets, ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	e := reflect.ValueOf(se).Elem().FieldByName("Emulator").Elem()
	scroll := e.FieldByName("scr").Elem().FieldByName("scroll")
	lo, hi := scroll.FieldByName("Min"), scroll.FieldByName("Max")
	m = Margins{
		Top: int(lo.FieldByName("Y").Int()) + 1, Bottom: int(hi.FieldByName("Y").Int()),
		Left: int(lo.FieldByName("X").Int()) + 1, Right: int(hi.FieldByName("X").Int()),
	}
	sets := e.FieldByName("charsets")
	for i := range cs.G {
		cs.G[i] = charsetFinal(sets.Index(i))
	}
	cs.GL = int(e.FieldByName("gl").Int())
	cs.GR = int(e.FieldByName("gr").Int())
	return m, cs, true
}

// charsetFinal names a designated set by its SCS final byte. vt
// designates by assigning its package-level maps, so identity tells
// them apart.
func charsetFinal(v reflect.Value) byte {
	switch {
	case v.IsNil():
		return 'B'
	case v.Pointer() == reflect.ValueOf(vt.UK).Pointer():
		return 'A'
	case v.Pointer() == reflect.ValueOf(vt.SpecialDrawing).Pointer():
		return '0'
	}
	return 'B'
}

// replayEmuExtras restores what the mode lists do not cover, after
// the screen cells: charsets, then the margins (DECSTBM and DECSLRM
// home the cursor, so the caller positions it afterwards).
func replayEmuExtras(emu *vt.SafeEmulator, spec AdoptSpec) {
	var b []byte
	if cs := spec.Charsets; cs != nil {
		for i, final := range cs.G {
			if final != 'B' {
				b = append(b, 0x1b, "()*+"[i], final)
			}
		}
		switch cs.GL {
		case 1:
			b = append(b, 0x0e) // SO
		case 2:
			b = append(b, 0x1b, 'n') // LS2
		case 3:
			b = append(b, 0x1b, 'o') // LS3
		}
		switch cs.GR {
		case 1:
			b = append(b, 0x1b, '~') // LS1R
		case 2:
			b = append(b, 0x1b, '}') // LS2R
		case 3:
			b = append(b, 0x1b, '|') // LS3R
		}
	}
	if m := spec.Margins; m != nil {
		b = append(b, ansi.SetTopBottomMargins(m.Top, m.Bottom)...)
		// Without DECLRMM, CSI s is SCOSC (save cursor), not DECSLRM.
		if modeIn(spec.DECModesSet, int(ansi.ModeLeftRightMargin)) {
			b = append(b, ansi.SetLeftRightMargins(m.Left, m.Right)...)
		}
	}
	if len(b) > 0 {
		_, _ = emu.Write(b)
	}
}

func modeIn(modes []int, m int) bool {
	for _, x := range modes {
		if x == m {
			return true
		}
	}
	return false
}

// observeStateSequences makes the escape sequences that change replay
// state without being modes (margins, charset designations and
// shifts, RIS) fire the state-change hook too. Each observer returns
// false, so vt's own handler still runs after it.
func (t *Terminal) observeStateSequences() {
	observe := func() bool {
		t.stateChanged()
		return false
	}
	for _, cmd := range []int{'r', 's'} {
		t.Emu.RegisterCsiHandler(cmd, func(ansi.Params) bool { return observe() })
	}
	for _, inter := range []byte("()*+") {
		for _, final := range []byte("AB0") {
			t.Emu.RegisterEscHandler(ansi.Command(0, inter, final), observe)
		}
	}
	for _, final := range []byte("cno|}~") {
		t.Emu.RegisterEscHandler(int(final), observe)
	}
}

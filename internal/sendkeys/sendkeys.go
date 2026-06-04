// Package sendkeys turns human-readable key tokens ("enter",
// "ctrl+c", "shift+up", "ctrl++") into the byte sequences a terminal
// application expects. It exists for the MCP send_keys tool: agents
// reliably fail at expressing keystrokes as raw escaped bytes (the
// JSON-escaping guessing game around "\r" alone caused real loops),
// but they're very good at writing "enter".
//
// Token grammar, tmux-inspired:
//
//   - modifiers prefix the key, joined by "+" or "-":
//     ctrl+c, alt+enter, ctrl+shift+up, C-M-x (tmux aliases work)
//   - prefixes strip greedily left to right; WHATEVER REMAINS is the
//     key — so "ctrl++" is ctrl plus '+', "ctrl--" is ctrl plus '-'.
//     No escaping rules, the grammar can't express them wrong.
//   - the key is either a single literal character (case kept) or a
//     named key: enter, esc, tab, backspace, space, delete, insert,
//     up, down, left, right, home, end, pageup, pagedown, f1..f12.
//
// Encoding: classic sequences where they exist (ctrl+letter →
// control byte, alt+x → ESC x, shift+tab → CSI Z, modified arrows →
// xterm CSI 1;m), and CSI-u (\x1b[<code>;<mods>u) for chords that
// have NO classic encoding (ctrl++, ctrl+enter, …) — modern TUIs
// understand it, everything else ignores the unknown CSI, which
// beats silently sending the wrong bytes.
package sendkeys

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// modifier bits, xterm convention: encoded parameter is 1+bits.
const (
	modShift = 1
	modAlt   = 2
	modCtrl  = 4
)

var modNames = map[string]int{
	"ctrl": modCtrl, "control": modCtrl, "c": modCtrl,
	"alt": modAlt, "meta": modAlt, "m": modAlt, "a": modAlt,
	"shift": modShift, "s": modShift,
}

// namedKey describes a key with no literal character. seq/appSeq are
// the unmodified encodings (appSeq used when DECCKM app-cursor mode
// is on; empty = same as seq). csi/tilde drive the modified forms:
// csi != 0 → "\x1b[1;<m><csi>"; tilde != 0 → "\x1b[<tilde>;<m>~".
// code is the CSI-u codepoint for keys whose modified form has no
// CSI/tilde encoding (enter, tab, …).
type namedKey struct {
	seq    string
	appSeq string
	csi    byte
	tilde  int
	code   int
}

var namedKeys = map[string]namedKey{
	"enter":     {seq: "\r", code: 13},
	"return":    {seq: "\r", code: 13},
	"esc":       {seq: "\x1b", code: 27},
	"escape":    {seq: "\x1b", code: 27},
	"tab":       {seq: "\t", code: 9},
	"backspace": {seq: "\x7f", code: 127},
	"bspace":    {seq: "\x7f", code: 127},
	"space":     {seq: " ", code: 32},
	"up":        {seq: "\x1b[A", appSeq: "\x1bOA", csi: 'A'},
	"down":      {seq: "\x1b[B", appSeq: "\x1bOB", csi: 'B'},
	"right":     {seq: "\x1b[C", appSeq: "\x1bOC", csi: 'C'},
	"left":      {seq: "\x1b[D", appSeq: "\x1bOD", csi: 'D'},
	"home":      {seq: "\x1b[H", appSeq: "\x1bOH", csi: 'H'},
	"end":       {seq: "\x1b[F", appSeq: "\x1bOF", csi: 'F'},
	"insert":    {seq: "\x1b[2~", tilde: 2},
	"delete":    {seq: "\x1b[3~", tilde: 3},
	"pageup":    {seq: "\x1b[5~", tilde: 5},
	"pgup":      {seq: "\x1b[5~", tilde: 5},
	"pagedown":  {seq: "\x1b[6~", tilde: 6},
	"pgdn":      {seq: "\x1b[6~", tilde: 6},
	"f1":        {seq: "\x1bOP", csi: 'P'},
	"f2":        {seq: "\x1bOQ", csi: 'Q'},
	"f3":        {seq: "\x1bOR", csi: 'R'},
	"f4":        {seq: "\x1bOS", csi: 'S'},
	"f5":        {seq: "\x1b[15~", tilde: 15},
	"f6":        {seq: "\x1b[17~", tilde: 17},
	"f7":        {seq: "\x1b[18~", tilde: 18},
	"f8":        {seq: "\x1b[19~", tilde: 19},
	"f9":        {seq: "\x1b[20~", tilde: 20},
	"f10":       {seq: "\x1b[21~", tilde: 21},
	"f11":       {seq: "\x1b[23~", tilde: 23},
	"f12":       {seq: "\x1b[24~", tilde: 24},
}

// Vocabulary returns the named-key tokens, sorted — used in error
// messages and tool docs so agents self-correct in one round.
func Vocabulary() []string {
	out := make([]string, 0, len(namedKeys))
	for k := range namedKeys {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Translate converts key tokens to the bytes to write to the PTY.
// appCursor is the tab's live DECCKM state (arrows/home/end encode
// SS3 instead of CSI when set). Unknown tokens error loudly.
func Translate(tokens []string, appCursor bool) ([]byte, error) {
	var out []byte
	for _, tok := range tokens {
		b, err := translateOne(tok, appCursor)
		if err != nil {
			return nil, err
		}
		out = append(out, b...)
	}
	return out, nil
}

func translateOne(token string, appCursor bool) ([]byte, error) {
	if token == "" {
		return nil, fmt.Errorf("empty key token")
	}
	// Strip modifier prefixes greedily. Case-insensitive; the
	// remainder after the last modifier is the key, verbatim — so
	// "ctrl++" resolves to ctrl + '+'.
	mods := 0
	rest := token
	for {
		stripped := false
		lower := strings.ToLower(rest)
		for name, bit := range modNames {
			if strings.HasPrefix(lower, name) && len(rest) > len(name)+1 {
				if j := rest[len(name)]; j == '+' || j == '-' {
					mods |= bit
					rest = rest[len(name)+1:]
					stripped = true
					break
				}
			}
		}
		if !stripped {
			break
		}
	}

	if nk, ok := namedKeys[strings.ToLower(rest)]; ok {
		return encodeNamed(nk, mods, appCursor), nil
	}
	if r, size := utf8.DecodeRuneInString(rest); size == len(rest) && r != utf8.RuneError {
		return encodeChar(r, mods), nil
	}
	return nil, fmt.Errorf(
		"unknown key %q in token %q — use a single character or one of: %s (modifiers: ctrl+/alt+/shift+, tmux C-/M-/S- also accepted)",
		rest, token, strings.Join(Vocabulary(), " "))
}

func encodeNamed(nk namedKey, mods int, appCursor bool) []byte {
	if mods == 0 {
		if appCursor && nk.appSeq != "" {
			return []byte(nk.appSeq)
		}
		return []byte(nk.seq)
	}
	m := 1 + mods
	switch {
	case nk.csi != 0:
		// xterm modified special key: CSI 1;m X. App-cursor mode is
		// irrelevant here — modified arrows always use CSI.
		return []byte(fmt.Sprintf("\x1b[1;%d%c", m, nk.csi))
	case nk.tilde != 0:
		return []byte(fmt.Sprintf("\x1b[%d;%d~", nk.tilde, m))
	default:
		// enter/tab/backspace/esc/space with modifiers. Classic
		// encodings exist for a few:
		if mods == modAlt {
			return append([]byte("\x1b"), nk.seq...) // alt+enter → ESC CR etc.
		}
		if mods == modShift && nk.code == 9 {
			return []byte("\x1b[Z") // shift+tab — back-tab
		}
		if mods == modCtrl && nk.code == 32 {
			return []byte{0} // ctrl+space — NUL
		}
		// No classic form (ctrl+enter, ctrl+shift+tab, …): CSI-u.
		return []byte(fmt.Sprintf("\x1b[%d;%du", nk.code, m))
	}
}

func encodeChar(r rune, mods int) []byte {
	if mods&modShift != 0 && unicode.IsLetter(r) {
		r = unicode.ToUpper(r)
		mods &^= modShift
	}
	var base []byte
	switch {
	case mods&modCtrl != 0:
		if c, ok := ctrlByte(r); ok {
			base = []byte{c}
			mods &^= modCtrl
		} else {
			// ctrl+<char> with no control byte (ctrl++, ctrl+1, …):
			// CSI-u carries the full chord, other mod bits included.
			return []byte(fmt.Sprintf("\x1b[%d;%du", r, 1+mods))
		}
	default:
		base = []byte(string(r))
	}
	if mods&modAlt != 0 {
		return append([]byte("\x1b"), base...)
	}
	return base
}

// ctrlByte returns the classic control byte for ctrl+<r>, if one
// exists: letters map to 0x01–0x1A, plus the @[\]^_? punctuation set.
func ctrlByte(r rune) (byte, bool) {
	switch {
	case r >= 'a' && r <= 'z':
		return byte(r) & 0x1f, true
	case r >= 'A' && r <= 'Z':
		return byte(r) & 0x1f, true
	case r == '@':
		return 0, true
	case r == '[':
		return 0x1b, true
	case r == '\\':
		return 0x1c, true
	case r == ']':
		return 0x1d, true
	case r == '^':
		return 0x1e, true
	case r == '_':
		return 0x1f, true
	case r == '?':
		return 0x7f, true
	}
	return 0, false
}

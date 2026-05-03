// Package input handles ImGui key → VT escape sequence translation.
package input

import (
	"runtime"

	"github.com/AllenDang/cimgui-go/imgui"
	"github.com/LXXero/xerotty/internal/sdlhack"
)

// KeyEvent represents a translated key event ready to send to the PTY.
type KeyEvent struct {
	Bytes  []byte // VT sequence or UTF-8 bytes to write to PTY
	Action string // keybind action name, empty if not a keybind
}

// PollKeys checks ImGui's key state and returns all pending key events.
// This replaces the broken SDL key callback approach.
func PollKeys(keybinds map[string]string, appMode bool) []KeyEvent {
	ctrl := imgui.IsKeyDown(imgui.ModCtrl)
	shift := imgui.IsKeyDown(imgui.ModShift)
	alt := imgui.IsKeyDown(imgui.ModAlt)
	super := imgui.IsKeyDown(imgui.ModSuper)

	// On macOS, ImGui's ConfigMacOSXBehaviors swaps ImGuiMod_Ctrl <->
	// ImGuiMod_Super internally so cross-platform shortcuts written as
	// "Ctrl+X" fire on physical Cmd. That swap is what keybinds rely on,
	// so leave it alone for matchKeybind. But terminal-bound Ctrl
	// behaviour — ASCII control codes (Ctrl+C → 0x03) and Ctrl+arrow
	// escape sequences — needs the physical Ctrl key, which arrives in
	// the cimgui-named `super` flag on Mac.
	ctrlPhysical := ctrl
	if runtime.GOOS == "darwin" {
		ctrlPhysical = super
	}

	var events []KeyEvent

	// Check keybinds first (Ctrl+Shift+F for search, etc.)
	for bind, action := range keybinds {
		if matchKeybind(bind, ctrl, shift, alt, super) {
			events = append(events, KeyEvent{Action: action})
			return events
		}
	}

	// Ctrl+letter → ASCII control codes (1-26), plus the non-letter
	// punctuation Ctrl combos every shell relies on:
	//   Ctrl+@ / Ctrl+Space → 0x00 (NUL — readline set-mark)
	//   Ctrl+[              → 0x1B (ESC — alternate to Escape key)
	//   Ctrl+\              → 0x1C (FS  — SIGQUIT)
	//   Ctrl+]              → 0x1D (GS  — telnet escape, readline abort)
	//   Ctrl+/              → 0x1F (US  — readline undo on some configs)
	// Shift-required combos (Ctrl+Shift+6 → ^ → 0x1E, Ctrl+Shift+- →
	// _ → 0x1F) are skipped because they collide with the keybind path
	// that uses Ctrl+Shift+...; users wanting RS/US can use Ctrl+/ for
	// 0x1F, and 0x1E is rarely used outside historical contexts.
	if ctrlPhysical && !alt && !shift {
		for k := imgui.KeyA; k <= imgui.KeyZ; k++ {
			if imgui.IsKeyPressedBool(k) {
				code := byte(k-imgui.KeyA) + 1
				events = append(events, KeyEvent{Bytes: []byte{code}})
			}
		}
		punct := []struct {
			key  imgui.Key
			code byte
		}{
			{imgui.KeySpace, 0x00},
			{imgui.KeyLeftBracket, 0x1B},
			{imgui.KeyBackslash, 0x1C},
			{imgui.KeyRightBracket, 0x1D},
			{imgui.KeySlash, 0x1F},
		}
		for _, p := range punct {
			if imgui.IsKeyPressedBool(p.key) {
				events = append(events, KeyEvent{Bytes: []byte{p.code}})
			}
		}
		if len(events) > 0 {
			return events
		}
	}

	// Special keys
	type specialKey struct {
		key imgui.Key
		fn  func(ctrl, shift, alt, appMode bool) KeyEvent
	}

	enterFn := func(_, shift, _, _ bool) KeyEvent {
		if shift {
			return KeyEvent{Bytes: []byte("\n")}
		}
		return KeyEvent{Bytes: []byte("\r")}
	}
	specials := []specialKey{
		{imgui.KeyEnter, enterFn},
		// Keypad Enter is a separate key in SDL/ImGui — same PTY semantics.
		{imgui.KeyKeypadEnter, enterFn},
		{imgui.KeyBackspace, func(_, _, _, _ bool) KeyEvent {
			return KeyEvent{Bytes: []byte{0x7f}}
		}},
		{imgui.KeyTab, func(_, shift, _, _ bool) KeyEvent {
			if shift {
				return KeyEvent{Bytes: []byte("\x1b[Z")}
			}
			return KeyEvent{Bytes: []byte("\t")}
		}},
		{imgui.KeyEscape, func(_, _, _, _ bool) KeyEvent {
			return KeyEvent{Bytes: []byte{0x1b}}
		}},
		{imgui.KeyDelete, func(_, _, _, _ bool) KeyEvent {
			return KeyEvent{Bytes: []byte("\x1b[3~")}
		}},
		{imgui.KeyInsert, func(_, _, _, _ bool) KeyEvent {
			return KeyEvent{Bytes: []byte("\x1b[2~")}
		}},
		{imgui.KeyUpArrow, func(ctrl, shift, _, app bool) KeyEvent {
			return arrowKey('A', ctrl, shift, app)
		}},
		{imgui.KeyDownArrow, func(ctrl, shift, _, app bool) KeyEvent {
			return arrowKey('B', ctrl, shift, app)
		}},
		{imgui.KeyRightArrow, func(ctrl, shift, _, app bool) KeyEvent {
			return arrowKey('C', ctrl, shift, app)
		}},
		{imgui.KeyLeftArrow, func(ctrl, shift, _, app bool) KeyEvent {
			return arrowKey('D', ctrl, shift, app)
		}},
		{imgui.KeyHome, func(_, _, _, app bool) KeyEvent {
			if app {
				return KeyEvent{Bytes: []byte("\x1bOH")}
			}
			return KeyEvent{Bytes: []byte("\x1b[H")}
		}},
		{imgui.KeyEnd, func(_, _, _, app bool) KeyEvent {
			if app {
				return KeyEvent{Bytes: []byte("\x1bOF")}
			}
			return KeyEvent{Bytes: []byte("\x1b[F")}
		}},
		{imgui.KeyPageUp, func(_, shift, _, _ bool) KeyEvent {
			if shift {
				return KeyEvent{Action: "scroll_page_up"}
			}
			return KeyEvent{Bytes: []byte("\x1b[5~")}
		}},
		{imgui.KeyPageDown, func(_, shift, _, _ bool) KeyEvent {
			if shift {
				return KeyEvent{Action: "scroll_page_down"}
			}
			return KeyEvent{Bytes: []byte("\x1b[6~")}
		}},
	}

	// Keypad navigation when NumLock is OFF. SDL/ImGui report keypad keys
	// regardless of NumLock state (KeyKeypad8 always fires on physical
	// press), so we have to gate on NumLock to avoid double-emitting:
	// with NumLock ON, the OS produces a TEXTINPUT event for the digit
	// which writes through the InputQueueCharacters path; if we ALSO
	// emitted an arrow-key sequence here, the user would see both '8'
	// and Up arrow. With NumLock OFF, no TEXTINPUT fires for keypad
	// keys, so emitting the navigation sequence here is the only path.
	//
	// Skipped entries (KP 5, +, -, *, /, =, .): no standard navigation
	// equivalent. KP Enter is handled in `specials` above (always fires
	// regardless of NumLock state, same as Enter).
	if !sdlhack.NumLockOn() {
		specials = append(specials,
			specialKey{imgui.KeyKeypad8, func(c, s, _, app bool) KeyEvent { return arrowKey('A', c, s, app) }},
			specialKey{imgui.KeyKeypad2, func(c, s, _, app bool) KeyEvent { return arrowKey('B', c, s, app) }},
			specialKey{imgui.KeyKeypad6, func(c, s, _, app bool) KeyEvent { return arrowKey('C', c, s, app) }},
			specialKey{imgui.KeyKeypad4, func(c, s, _, app bool) KeyEvent { return arrowKey('D', c, s, app) }},
			specialKey{imgui.KeyKeypad7, func(_, _, _, app bool) KeyEvent {
				if app {
					return KeyEvent{Bytes: []byte("\x1bOH")}
				}
				return KeyEvent{Bytes: []byte("\x1b[H")}
			}},
			specialKey{imgui.KeyKeypad1, func(_, _, _, app bool) KeyEvent {
				if app {
					return KeyEvent{Bytes: []byte("\x1bOF")}
				}
				return KeyEvent{Bytes: []byte("\x1b[F")}
			}},
			specialKey{imgui.KeyKeypad9, func(_, shift, _, _ bool) KeyEvent {
				if shift {
					return KeyEvent{Action: "scroll_page_up"}
				}
				return KeyEvent{Bytes: []byte("\x1b[5~")}
			}},
			specialKey{imgui.KeyKeypad3, func(_, shift, _, _ bool) KeyEvent {
				if shift {
					return KeyEvent{Action: "scroll_page_down"}
				}
				return KeyEvent{Bytes: []byte("\x1b[6~")}
			}},
			specialKey{imgui.KeyKeypad0, func(_, _, _, _ bool) KeyEvent {
				return KeyEvent{Bytes: []byte("\x1b[2~")}
			}},
			specialKey{imgui.KeyKeypadDecimal, func(_, _, _, _ bool) KeyEvent {
				return KeyEvent{Bytes: []byte("\x1b[3~")}
			}},
		)
	}

	// Function keys
	fkeys := []struct {
		key imgui.Key
		seq string
	}{
		{imgui.KeyF1, "\x1bOP"}, {imgui.KeyF2, "\x1bOQ"},
		{imgui.KeyF3, "\x1bOR"}, {imgui.KeyF4, "\x1bOS"},
		{imgui.KeyF5, "\x1b[15~"}, {imgui.KeyF6, "\x1b[17~"},
		{imgui.KeyF7, "\x1b[18~"}, {imgui.KeyF8, "\x1b[19~"},
		{imgui.KeyF9, "\x1b[20~"}, {imgui.KeyF10, "\x1b[21~"},
		{imgui.KeyF11, "\x1b[23~"}, {imgui.KeyF12, "\x1b[24~"},
	}

	for _, sk := range specials {
		if imgui.IsKeyPressedBool(sk.key) {
			ev := sk.fn(ctrlPhysical, shift, alt, appMode)
			events = append(events, ev)
		}
	}

	for _, fk := range fkeys {
		if imgui.IsKeyPressedBool(fk.key) {
			events = append(events, KeyEvent{Bytes: []byte(fk.seq)})
		}
	}

	return events
}

func matchKeybind(bind string, ctrl, shift, alt, super bool) bool {
	wantCtrl := false
	wantShift := false
	wantAlt := false
	wantSuper := false
	keyPart := bind

	for {
		if len(keyPart) > 5 && keyPart[:5] == "Ctrl+" {
			wantCtrl = true
			keyPart = keyPart[5:]
		} else if len(keyPart) > 6 && keyPart[:6] == "Shift+" {
			wantShift = true
			keyPart = keyPart[6:]
		} else if len(keyPart) > 4 && keyPart[:4] == "Alt+" {
			wantAlt = true
			keyPart = keyPart[4:]
		} else if len(keyPart) > 4 && keyPart[:4] == "Cmd+" {
			wantSuper = true
			keyPart = keyPart[4:]
		} else if len(keyPart) > 6 && keyPart[:6] == "Super+" {
			wantSuper = true
			keyPart = keyPart[6:]
		} else {
			break
		}
	}

	if ctrl != wantCtrl || shift != wantShift || alt != wantAlt || super != wantSuper {
		return false
	}

	imKey := nameToImGuiKey(keyPart)
	if imKey == imgui.KeyNone {
		return false
	}
	return imgui.IsKeyPressedBool(imKey)
}

func nameToImGuiKey(name string) imgui.Key {
	switch name {
	case "A":
		return imgui.KeyA
	case "B":
		return imgui.KeyB
	case "C":
		return imgui.KeyC
	case "D":
		return imgui.KeyD
	case "E":
		return imgui.KeyE
	case "F":
		return imgui.KeyF
	case "G":
		return imgui.KeyG
	case "H":
		return imgui.KeyH
	case "I":
		return imgui.KeyI
	case "J":
		return imgui.KeyJ
	case "K":
		return imgui.KeyK
	case "L":
		return imgui.KeyL
	case "M":
		return imgui.KeyM
	case "N":
		return imgui.KeyN
	case "O":
		return imgui.KeyO
	case "P":
		return imgui.KeyP
	case "Q":
		return imgui.KeyQ
	case "R":
		return imgui.KeyR
	case "S":
		return imgui.KeyS
	case "T":
		return imgui.KeyT
	case "U":
		return imgui.KeyU
	case "V":
		return imgui.KeyV
	case "W":
		return imgui.KeyW
	case "X":
		return imgui.KeyX
	case "Y":
		return imgui.KeyY
	case "Z":
		return imgui.KeyZ
	case "0":
		return imgui.Key0
	case "1":
		return imgui.Key1
	case "2":
		return imgui.Key2
	case "3":
		return imgui.Key3
	case "4":
		return imgui.Key4
	case "5":
		return imgui.Key5
	case "6":
		return imgui.Key6
	case "7":
		return imgui.Key7
	case "8":
		return imgui.Key8
	case "9":
		return imgui.Key9
	case "Enter":
		return imgui.KeyEnter
	case "Tab":
		return imgui.KeyTab
	case "Backspace":
		return imgui.KeyBackspace
	case "Escape":
		return imgui.KeyEscape
	case "Space":
		return imgui.KeySpace
	case "Delete":
		return imgui.KeyDelete
	case "Insert":
		return imgui.KeyInsert
	case "Home":
		return imgui.KeyHome
	case "End":
		return imgui.KeyEnd
	case "PageUp":
		return imgui.KeyPageUp
	case "PageDown":
		return imgui.KeyPageDown
	case "Up":
		return imgui.KeyUpArrow
	case "Down":
		return imgui.KeyDownArrow
	case "Left":
		return imgui.KeyLeftArrow
	case "Right":
		return imgui.KeyRightArrow
	case "F1":
		return imgui.KeyF1
	case "F2":
		return imgui.KeyF2
	case "F3":
		return imgui.KeyF3
	case "F4":
		return imgui.KeyF4
	case "F5":
		return imgui.KeyF5
	case "F6":
		return imgui.KeyF6
	case "F7":
		return imgui.KeyF7
	case "F8":
		return imgui.KeyF8
	case "F9":
		return imgui.KeyF9
	case "F10":
		return imgui.KeyF10
	case "F11":
		return imgui.KeyF11
	case "F12":
		return imgui.KeyF12
	case "Minus":
		return imgui.KeyMinus
	case "Plus":
		return imgui.KeyEqual
	}
	return imgui.KeyNone
}

func arrowKey(dir byte, ctrl, shift, appMode bool) KeyEvent {
	if ctrl {
		return KeyEvent{Bytes: []byte{0x1b, '[', '1', ';', '5', dir}}
	}
	if shift {
		return KeyEvent{Bytes: []byte{0x1b, '[', '1', ';', '2', dir}}
	}
	if appMode {
		return KeyEvent{Bytes: []byte{0x1b, 'O', dir}}
	}
	return KeyEvent{Bytes: []byte{0x1b, '[', dir}}
}

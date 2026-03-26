// Package input handles SDL key → VT escape sequence translation and modifier tracking.
package input

// KeyEvent represents a translated key event ready to send to the PTY.
type KeyEvent struct {
	Bytes []byte // VT sequence or UTF-8 bytes to write to PTY
	Action string // keybind action name, empty if not a keybind
}

// Translate converts an SDL key event into bytes for the PTY or a keybind action.
// key is the SDL keycode, mods is the modifier bitmask, text is the SDL text input string.
func Translate(key, mods int, text string, keybinds map[string]string, appMode bool) KeyEvent {
	ctrl := mods&ModCtrl != 0
	shift := mods&ModShift != 0
	alt := mods&ModAlt != 0

	// Build keybind string and check
	bindStr := modString(ctrl, shift, alt, key)
	if bindStr != "" {
		if action, ok := keybinds[bindStr]; ok {
			return KeyEvent{Action: action}
		}
	}

	// Ctrl+letter → ASCII control codes (1-26)
	if ctrl && !alt && !shift && key >= KeyA && key <= KeyZ {
		return KeyEvent{Bytes: []byte{byte(key - KeyA + 1)}}
	}

	// Alt+key → ESC prefix
	if alt && !ctrl {
		if text != "" {
			return KeyEvent{Bytes: append([]byte{0x1b}, []byte(text)...)}
		}
	}

	// Special keys
	switch key {
	case KeyReturn:
		if shift {
			return KeyEvent{Bytes: []byte("\n")}
		}
		return KeyEvent{Bytes: []byte("\r")}
	case KeyBackspace:
		return KeyEvent{Bytes: []byte{0x7f}}
	case KeyTab:
		if shift {
			return KeyEvent{Bytes: []byte("\x1b[Z")}
		}
		return KeyEvent{Bytes: []byte("\t")}
	case KeyEscape:
		return KeyEvent{Bytes: []byte{0x1b}}
	case KeyDelete:
		return KeyEvent{Bytes: []byte("\x1b[3~")}
	case KeyInsert:
		return KeyEvent{Bytes: []byte("\x1b[2~")}

	// Arrow keys
	case KeyUp:
		return arrowKey('A', ctrl, shift, appMode)
	case KeyDown:
		return arrowKey('B', ctrl, shift, appMode)
	case KeyRight:
		return arrowKey('C', ctrl, shift, appMode)
	case KeyLeft:
		return arrowKey('D', ctrl, shift, appMode)

	// Navigation
	case KeyHome:
		if appMode {
			return KeyEvent{Bytes: []byte("\x1bOH")}
		}
		return KeyEvent{Bytes: []byte("\x1b[H")}
	case KeyEnd:
		if appMode {
			return KeyEvent{Bytes: []byte("\x1bOF")}
		}
		return KeyEvent{Bytes: []byte("\x1b[F")}
	case KeyPageUp:
		if shift {
			return KeyEvent{Action: "scroll_page_up"}
		}
		return KeyEvent{Bytes: []byte("\x1b[5~")}
	case KeyPageDown:
		if shift {
			return KeyEvent{Action: "scroll_page_down"}
		}
		return KeyEvent{Bytes: []byte("\x1b[6~")}

	// Function keys
	case KeyF1:
		return KeyEvent{Bytes: []byte("\x1bOP")}
	case KeyF2:
		return KeyEvent{Bytes: []byte("\x1bOQ")}
	case KeyF3:
		return KeyEvent{Bytes: []byte("\x1bOR")}
	case KeyF4:
		return KeyEvent{Bytes: []byte("\x1bOS")}
	case KeyF5:
		return KeyEvent{Bytes: []byte("\x1b[15~")}
	case KeyF6:
		return KeyEvent{Bytes: []byte("\x1b[17~")}
	case KeyF7:
		return KeyEvent{Bytes: []byte("\x1b[18~")}
	case KeyF8:
		return KeyEvent{Bytes: []byte("\x1b[19~")}
	case KeyF9:
		return KeyEvent{Bytes: []byte("\x1b[20~")}
	case KeyF10:
		return KeyEvent{Bytes: []byte("\x1b[21~")}
	case KeyF11:
		return KeyEvent{Bytes: []byte("\x1b[23~")}
	case KeyF12:
		return KeyEvent{Bytes: []byte("\x1b[24~")}
	}

	// Regular text input
	if text != "" && !ctrl {
		return KeyEvent{Bytes: []byte(text)}
	}

	return KeyEvent{}
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

func modString(ctrl, shift, alt bool, key int) string {
	s := ""
	if ctrl {
		s += "Ctrl+"
	}
	if shift {
		s += "Shift+"
	}
	if alt {
		s += "Alt+"
	}

	name := keyName(key)
	if name == "" {
		return ""
	}
	return s + name
}

func keyName(key int) string {
	switch {
	case key >= KeyA && key <= KeyZ:
		return string(rune('A' + (key - KeyA)))
	case key >= Key0 && key <= Key9:
		return string(rune('0' + (key - Key0)))
	}

	switch key {
	case KeyReturn:
		return "Enter"
	case KeyTab:
		return "Tab"
	case KeyBackspace:
		return "Backspace"
	case KeyEscape:
		return "Escape"
	case KeySpace:
		return "Space"
	case KeyDelete:
		return "Delete"
	case KeyInsert:
		return "Insert"
	case KeyHome:
		return "Home"
	case KeyEnd:
		return "End"
	case KeyPageUp:
		return "PageUp"
	case KeyPageDown:
		return "PageDown"
	case KeyUp:
		return "Up"
	case KeyDown:
		return "Down"
	case KeyLeft:
		return "Left"
	case KeyRight:
		return "Right"
	case KeyF1:
		return "F1"
	case KeyF2:
		return "F2"
	case KeyF3:
		return "F3"
	case KeyF4:
		return "F4"
	case KeyF5:
		return "F5"
	case KeyF6:
		return "F6"
	case KeyF7:
		return "F7"
	case KeyF8:
		return "F8"
	case KeyF9:
		return "F9"
	case KeyF10:
		return "F10"
	case KeyF11:
		return "F11"
	case KeyF12:
		return "F12"
	case KeyMinus:
		return "Minus"
	case KeyEquals:
		return "Plus" // Shift+= is + on most layouts
	}
	return ""
}

// SDL2 key constants (SDLK_* values).
const (
	ModShift = 0x0001
	ModCtrl  = 0x0040
	ModAlt   = 0x0100

	KeyReturn    = 13
	KeyEscape    = 27
	KeyBackspace = 8
	KeyTab       = 9
	KeySpace     = 32
	KeyDelete    = 127

	KeyA = 'a'
	KeyZ = 'z'
	Key0 = '0'
	Key9 = '9'

	KeyMinus  = '-'
	KeyEquals = '='

	// SDL scancodes for special keys (these are SDLK_ values)
	KeyInsert   = 1073741897
	KeyHome     = 1073741898
	KeyEnd      = 1073741901
	KeyPageUp   = 1073741899
	KeyPageDown = 1073741902

	KeyRight = 1073741903
	KeyLeft  = 1073741904
	KeyDown  = 1073741905
	KeyUp    = 1073741906

	KeyF1  = 1073741882
	KeyF2  = 1073741883
	KeyF3  = 1073741884
	KeyF4  = 1073741885
	KeyF5  = 1073741886
	KeyF6  = 1073741887
	KeyF7  = 1073741888
	KeyF8  = 1073741889
	KeyF9  = 1073741890
	KeyF10 = 1073741891
	KeyF11 = 1073741892
	KeyF12 = 1073741893
)

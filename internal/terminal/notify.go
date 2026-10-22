package terminal

import (
	"encoding/base64"
	"strings"
	"unicode/utf8"
)

// Notification is a desktop notification a program asked the terminal
// to show, parsed from one of the three OSC forms in use:
//
//   - OSC 9 ; <body>                     iTerm2 (also Ghostty, WezTerm)
//   - OSC 777 ; notify ; <title> ; <body>  rxvt-unicode, foot, Ghostty
//   - OSC 99 ; <metadata> ; <payload>    kitty desktop notifications
//
// xerotty does not display these; the daemon records them as tab
// events so an agent watching the fleet learns that a program wanted
// attention.
type Notification struct {
	// Protocol is "osc9", "osc777" or "osc99".
	Protocol string
	Title    string
	Body     string
}

// notifyFieldMax caps the title and body we keep from a notification.
// The text is program-supplied and lands in a bounded in-memory event
// ring; a few hundred bytes is plenty for "Build finished" style
// messages and stops one program from parking megabytes there.
const notifyFieldMax = 512

// kittyPendingMax bounds how many unfinished (d=0) kitty notifications
// we hold while waiting for their final chunk. A program that never
// sends d=1 must not grow this without limit.
const kittyPendingMax = 8

// kittyNotifyAssembler joins the chunks of kitty OSC 99 notifications.
// Only the PTY reader goroutine touches it (via dispatchOSC), so it
// needs no lock.
type kittyNotifyAssembler struct {
	pending map[string]*Notification
	order   []string // ids in arrival order, oldest first, for eviction
}

// parseOSC9 handles an OSC 9 body (after "9;"). ConEmu reuses OSC 9
// with a numeric subcommand ("9;4;1;50" is its progress bar, which
// many CLIs emit); those are not notifications, so a body that starts
// with digits followed by ';' or the end is dropped.
func parseOSC9(data []byte) (Notification, bool) {
	s := string(data)
	if s == "" || isConEmuSubcommand(s) {
		return Notification{}, false
	}
	return Notification{Protocol: "osc9", Body: clampNotifyField(s)}, true
}

func isConEmuSubcommand(s string) bool {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return i > 0 && (i == len(s) || s[i] == ';')
}

// parseOSC777 handles an OSC 777 body (after "777;"). Only the
// "notify" subcommand is a notification; the body may itself contain
// ';', so everything after the title is the body.
func parseOSC777(data []byte) (Notification, bool) {
	parts := strings.SplitN(string(data), ";", 3)
	if len(parts) < 2 || parts[0] != "notify" {
		return Notification{}, false
	}
	n := Notification{Protocol: "osc777", Title: clampNotifyField(parts[1])}
	if len(parts) == 3 {
		n.Body = clampNotifyField(parts[2])
	}
	if n.Title == "" && n.Body == "" {
		return Notification{}, false
	}
	return n, true
}

// add handles an OSC 99 body (after "99;"): "<metadata>;<payload>",
// metadata being ':'-separated key=value pairs. The keys used here:
// i (notification id), d (0 = more chunks follow, default 1 = done),
// p (payload type: "title" (default) or "body"; others such as close,
// icon, buttons or the "?" capability query carry no text and are
// ignored) and e (1 = payload is base64). Returns the notification
// once its final chunk arrives.
func (k *kittyNotifyAssembler) add(data []byte) (Notification, bool) {
	semi := strings.IndexByte(string(data), ';')
	if semi < 0 {
		return Notification{}, false
	}
	meta, payload := string(data[:semi]), string(data[semi+1:])
	id, done, kind, b64 := "", true, "title", false
	for _, kv := range strings.Split(meta, ":") {
		key, val, _ := strings.Cut(kv, "=")
		switch key {
		case "i":
			id = val
		case "d":
			done = val != "0"
		case "p":
			kind = val
		case "e":
			b64 = val == "1"
		}
	}
	if b64 {
		dec, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return Notification{}, false
		}
		payload = string(dec)
	}

	n := k.pending[id]
	if n == nil {
		n = &Notification{Protocol: "osc99"}
	}
	switch kind {
	case "title":
		n.Title = clampNotifyField(n.Title + payload)
	case "body":
		n.Body = clampNotifyField(n.Body + payload)
	default:
		// No text in this chunk. A lone non-text chunk (a close or a
		// capability query) must not produce an empty notification.
		if k.pending[id] == nil {
			return Notification{}, false
		}
	}

	if !done {
		k.hold(id, n)
		return Notification{}, false
	}
	k.drop(id)
	if n.Title == "" && n.Body == "" {
		return Notification{}, false
	}
	return *n, true
}

func (k *kittyNotifyAssembler) hold(id string, n *Notification) {
	if k.pending == nil {
		k.pending = make(map[string]*Notification)
	}
	if _, ok := k.pending[id]; !ok {
		if len(k.order) >= kittyPendingMax {
			delete(k.pending, k.order[0])
			k.order = k.order[1:]
		}
		k.order = append(k.order, id)
	}
	k.pending[id] = n
}

func (k *kittyNotifyAssembler) drop(id string) {
	if _, ok := k.pending[id]; !ok {
		return
	}
	delete(k.pending, id)
	for i, o := range k.order {
		if o == id {
			k.order = append(k.order[:i], k.order[i+1:]...)
			break
		}
	}
}

// clampNotifyField cuts s to notifyFieldMax bytes without splitting a
// UTF-8 sequence.
func clampNotifyField(s string) string {
	if len(s) <= notifyFieldMax {
		return s
	}
	s = s[:notifyFieldMax]
	// Back off at most a partial rune's worth of bytes; invalid
	// UTF-8 earlier in the text is the program's and stays as is.
	for i := 0; i < utf8.UTFMax-1 && len(s) > 0; i++ {
		if r, size := utf8.DecodeLastRuneInString(s); r != utf8.RuneError || size > 1 {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}

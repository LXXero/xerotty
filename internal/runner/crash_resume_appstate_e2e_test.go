package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// appScript is a stand-in for mutt: after the last topology push it
// switches to the alt screen, turns on app cursor keys and keypad
// mode, sets a scroll region (rows 3-10) with origin mode, marks row 1
// outside the region, and blocks reading one key in raw mode. Then it
// prints the key's bytes and 30 lines, which must scroll inside the
// region only.
const appScript = `printf '\033[?1049h\033[?1h\033=\033[3;10r\033[1;1HTOPMARK\033[?6h\033[1;1HREADY'
stty raw -echo
k=$(dd bs=1 count=3 2>/dev/null | od -An -tx1 | tr -d ' \n')
stty sane
i=0; while [ $i -lt 30 ]; do printf '\r\nROW_%s' $i; i=$((i+1)); done
printf '\r\nKEY_%s' "$k"
read x
`

// TestCrashResumeRestoresAppStateE2E: an app sets DECCKM, a scroll
// region and the alt screen after the last topology push; the daemon
// child is SIGKILLed; the crash resume must bring back the screen, the
// modes and the margins: Up arrives as ESC O A, and output scrolls
// inside the region, leaving row 1 alone. Before, the supervisor held
// only topology, so the resumed tab had normal cursor keys (mutt's
// arrows misread) and no scroll region (windows scrolled whole).
func TestCrashResumeRestoresAppStateE2E(t *testing.T) {
	f := startUpgradeFixture(t, nil)
	script := filepath.Join(t.TempDir(), "app.sh")
	if err := os.WriteFile(script, []byte(appScript), 0o644); err != nil {
		t.Fatal(err)
	}
	p := f.probe()
	if _, err := p.call("tab/input", map[string]any{"tab_id": f.tabID, "bytes": "sh " + script + "\r"}); err != nil {
		t.Fatal(err)
	}
	f.waitScreen(p, func(scr string) bool { return strings.Contains(scr, "READY") }, "app never started")
	// Mode changes push within a debounce; the screen within a second.
	time.Sleep(2 * time.Second)

	sup := f.srv.Process.Pid
	child := childOf(t, sup)
	if err := syscall.Kill(child, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for childOf(t, sup) == child {
		if time.Now().After(deadline) {
			t.Fatal("supervisor never replaced the killed child")
		}
		time.Sleep(100 * time.Millisecond)
	}
	p = f.probe()
	// The screen came back from the crash state, not from a repaint:
	// this app never redraws.
	f.waitScreen(p, func(scr string) bool {
		return strings.Contains(scr, "TOPMARK") && strings.Contains(scr, "READY")
	}, "screen not restored by the crash resume")

	if _, err := p.call("tab/keys", map[string]any{"tab_id": f.tabID, "keys": []string{"Up"}}); err != nil {
		t.Fatalf("tab/keys: %v", err)
	}
	f.waitScreen(p, func(scr string) bool { return strings.Contains(scr, "KEY_") }, "app never got the key")
	res, err := p.call("tab/screen", map[string]any{"tab_id": f.tabID})
	if err != nil {
		t.Fatal(err)
	}
	var sc struct {
		Lines []string `json:"lines"`
	}
	if err := json.Unmarshal(res, &sc); err != nil {
		t.Fatal(err)
	}
	scr := strings.Join(sc.Lines, "\n")
	if !strings.Contains(scr, "KEY_1b4f41") {
		t.Fatalf("Up did not arrive as ESC O A (app cursor mode lost):\n%s", scr)
	}
	if len(sc.Lines) < 10 || !strings.Contains(sc.Lines[0], "TOPMARK") {
		t.Fatalf("row 1 scrolled away: the scroll region was lost:\n%s", scr)
	}
	if !strings.Contains(sc.Lines[8], "ROW_29") || !strings.Contains(sc.Lines[9], "KEY_") {
		t.Fatalf("output did not end at the region's bottom (row 10):\n%s", scr)
	}
}

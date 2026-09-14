package app

import (
	"strings"
	"testing"

	"github.com/LXXero/xerotty/internal/config"
)

// TestResolveAction pins the invocation grammar: bare ids for NoArg
// actions, "id:arg" (first colon splits) for arg-taking ones, and
// strict rejection of the mismatched forms — same strictness the old
// dispatchAction switch + prefix checks had.
func TestResolveAction(t *testing.T) {
	cases := []struct {
		in      string
		wantOK  bool
		wantID  string
		wantArg string
	}{
		{"new_tab", true, "new_tab", ""},
		{"goto_tab:3", true, "goto_tab", "3"},
		{"set_theme:dracula", true, "set_theme", "dracula"},
		// arg may itself contain colons — only the FIRST splits.
		{"kick_client:local:xerotty-gui:xryzen", true, "kick_client", "local:xerotty-gui:xryzen"},
		{"exec:notify-send hi", true, "exec", "notify-send hi"},
		// arg-taking action without an arg: no match (old behavior:
		// bare "goto_tab" fell through the prefix check to nothing).
		{"goto_tab", false, "", ""},
		// NoArg action with a stray arg: no match.
		{"new_tab:junk", false, "", ""},
		{"definitely_not_an_action", false, "", ""},
		{"", false, "", ""},
		{":", false, "", ""},
	}
	for _, c := range cases {
		a, arg, ok := resolveAction(c.in)
		if ok != c.wantOK {
			t.Errorf("resolveAction(%q) ok = %v, want %v", c.in, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if a.ID != c.wantID || arg != c.wantArg {
			t.Errorf("resolveAction(%q) = (%s, %q), want (%s, %q)",
				c.in, a.ID, arg, c.wantID, c.wantArg)
		}
	}
}

// TestDefaultConfigActionsResolve walks the DEFAULT keybinds and menu
// and requires every reference to resolve against the registry — the
// guard that keeps a new default binding or menu entry from shipping
// with an unregistered (dead) action, and a registered action from
// being removed while defaults still reference it.
func TestDefaultConfigActionsResolve(t *testing.T) {
	cfg := config.Default()
	for chord, act := range cfg.Keybinds {
		if !KnownAction(act) {
			t.Errorf("default keybind %s -> unregistered action %q", chord, act)
		}
	}
	var walk func(items []config.MenuItem, path string)
	walk = func(items []config.MenuItem, path string) {
		for _, it := range items {
			if len(it.Submenu) > 0 {
				walk(it.Submenu, path+it.Label+" > ")
				continue
			}
			if it.Action == "" || it.Action == "separator" || strings.HasPrefix(it.Action, "_") {
				continue
			}
			if !KnownAction(it.Action) {
				t.Errorf("default menu item %s%q -> unregistered action %q", path, it.Label, it.Action)
			}
		}
	}
	walk(cfg.Menu.Items, "")
}

// TestMenuAddOptionsMirrorRegistry pins the registry-derived Add
// combo: every NoArg action is offered (the old hand-maintained list
// had drifted — quit and the remote actions were missing), arg-taking
// actions are excluded (the combo has no arg field), and the menu
// grammar tokens ride along.
func TestMenuAddOptionsMirrorRegistry(t *testing.T) {
	prefMenuAddSorted = nil // force rebuild in case another test ran first
	ensureMenuAddOptions()
	have := map[string]bool{}
	for _, o := range prefMenuAddSorted {
		have[o.action] = true
	}
	for id, a := range actionRegistry {
		if a.Arg == NoArg && !have[id] {
			t.Errorf("NoArg action %q missing from menu Add options", id)
		}
		if a.Arg != NoArg && have[id] {
			t.Errorf("arg-taking action %q must not be in the Add combo (no arg field)", id)
		}
	}
	for _, tok := range []string{"separator", "_remote_hosts", menuKindSubmenu} {
		if !have[tok] {
			t.Errorf("menu grammar token %q missing from Add options", tok)
		}
	}
}

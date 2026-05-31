package app

import (
	"reflect"
	"testing"

	"github.com/LXXero/xerotty/internal/config"
)

// TestMenuEditorRoundTrip is the foundation regression: the prefs menu
// editor must carry submenus through a load (config -> editor) + apply
// (editor -> config) cycle unchanged. Before menuEditorItem grew a
// recursive submenu field the editor was flat, so loadFrom/applyTo
// silently DROPPED any [[menu.items.submenu]] on the first save.
func TestMenuEditorRoundTrip(t *testing.T) {
	items := []config.MenuItem{
		{Label: "New Tab", Action: "new_tab", Shortcut: "Ctrl+Shift+T"},
		{Action: "separator"},
		{Label: "Toggle Opacity", Action: "toggle_opacity", Checked: "force_opaque"},
		{
			Label: "Profiles",
			Submenu: []config.MenuItem{
				{Label: "Work", Action: "exec:work.sh"},
				{Label: "Home", Action: "exec:home.sh", Enabled: "always"},
				{
					Label: "Nested",
					Submenu: []config.MenuItem{
						{Label: "Deep", Action: "copy"},
					},
				},
			},
		},
	}

	got := editorToMenuItems(menuItemsToEditor(items))
	if !reflect.DeepEqual(got, items) {
		t.Fatalf("menu round-trip dropped/altered nested items:\n got: %#v\nwant: %#v", got, items)
	}
}

// TestMenuAddSortedMapping guards the CRITICAL Add-combo invariant: the
// combo displays friendly labels sorted alphabetically, but the selected
// index must still map back to the correct action. A reorder bug here
// would silently add the wrong menu item.
func TestMenuAddSortedMapping(t *testing.T) {
	// Labels must be sorted ascending.
	for i := 1; i < len(prefMenuAddLabels); i++ {
		if prefMenuAddLabels[i-1] > prefMenuAddLabels[i] {
			t.Fatalf("combo labels not sorted: %q before %q", prefMenuAddLabels[i-1], prefMenuAddLabels[i])
		}
	}
	// labels[i] must be the friendly label of the action menuAddSelection(i) returns.
	if len(prefMenuAddLabels) != len(prefMenuAddSorted) {
		t.Fatalf("label/option length mismatch: %d vs %d", len(prefMenuAddLabels), len(prefMenuAddSorted))
	}
	for i := range prefMenuAddSorted {
		action := menuAddSelection(int32(i))
		if menuAddLabel(action) != prefMenuAddLabels[i] {
			t.Fatalf("index %d shows %q but maps to action %q (label %q)",
				i, prefMenuAddLabels[i], action, menuAddLabel(action))
		}
	}
	// Every original option (incl. the _submenu sentinel) is still reachable.
	got := map[string]bool{}
	for i := range prefMenuAddSorted {
		got[menuAddSelection(int32(i))] = true
	}
	for _, a := range prefMenuAddOptions {
		if !got[a] {
			t.Fatalf("action %q dropped from the sorted Add combo", a)
		}
	}
	// Out-of-range never yields a wrong/garbage action.
	if menuAddSelection(-1) != prefMenuAddSorted[0].action || menuAddSelection(9999) != prefMenuAddSorted[0].action {
		t.Fatalf("out-of-range index did not fall back to the first option")
	}
}

// TestMenuEditorSubmenuKind checks that loading a config submenu marks
// the editor entry as a submenu (isSubmenu) with no action, while a leaf
// stays an action — the editor's three-kinds model must match what the
// engine infers from len(Submenu).
func TestMenuEditorSubmenuKind(t *testing.T) {
	items := []config.MenuItem{
		{Label: "Copy", Action: "copy"},
		{Label: "Tools", Submenu: []config.MenuItem{{Label: "Top", Action: "scroll_top"}}},
	}
	ed := menuItemsToEditor(items)
	if ed[0].isSubmenu || ed[0].action != "copy" {
		t.Fatalf("leaf misclassified: %+v", ed[0])
	}
	if !ed[1].isSubmenu {
		t.Fatalf("submenu not flagged isSubmenu: %+v", ed[1])
	}
	if ed[1].action != "" {
		t.Fatalf("submenu container must have no action of its own, got %q", ed[1].action)
	}
	if len(ed[1].submenu) != 1 || ed[1].submenu[0].action != "scroll_top" {
		t.Fatalf("submenu child not carried: %+v", ed[1].submenu)
	}
}

// TestMenuEditorEmptySubmenuDegrades verifies an editor-created EMPTY
// submenu (no children yet) round-trips sanely: config.MenuItem can't
// express it, so it saves as a leaf-with-label and no action, and
// reloads as a plain action entry (not a phantom submenu). Crucially it
// must NOT panic or resurrect as an isSubmenu with bogus state.
func TestMenuEditorEmptySubmenuDegrades(t *testing.T) {
	ed := []menuEditorItem{{label: "Empty", isSubmenu: true}}

	saved := editorToMenuItems(ed)
	want := []config.MenuItem{{Label: "Empty"}}
	if !reflect.DeepEqual(saved, want) {
		t.Fatalf("empty submenu did not degrade to a leaf:\n got: %#v\nwant: %#v", saved, want)
	}

	back := menuItemsToEditor(saved)
	if back[0].isSubmenu {
		t.Fatalf("degraded empty submenu reloaded as a submenu: %+v", back[0])
	}
	if len(back[0].submenu) != 0 {
		t.Fatalf("degraded empty submenu reloaded with children: %+v", back[0])
	}
}

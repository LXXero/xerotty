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

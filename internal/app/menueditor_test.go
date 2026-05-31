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

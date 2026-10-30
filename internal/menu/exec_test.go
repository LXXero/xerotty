package menu

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExecActionLinkIsNotShellText: an OSC 8 target is chosen by the
// program in the terminal, so no quoting in it may reach the shell as
// syntax, whichever way the template writes the variable.
func TestExecActionLinkIsNotShellText(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	link := "http://example.com/$(touch " + marker + ")';touch " + marker + ";'\";touch " + marker + ";\""

	for _, tmpl := range []string{
		`printf %s "$XEROTTY_LINK" > ` + dir + `/out`,
		`printf %s '$XEROTTY_LINK' > ` + dir + `/out`,
		`printf %s $XEROTTY_LINK > ` + dir + `/out`,
	} {
		cmd := execCommand("exec:"+tmpl, &Context{Link: link})
		if cmd == nil {
			t.Fatal("no command")
		}
		if err := cmd.Run(); err != nil {
			t.Fatalf("%s: %v", tmpl, err)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("%s: the link ran as shell code", tmpl)
		}
		got, err := os.ReadFile(filepath.Join(dir, "out"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(got), "http://example.com/$(touch") {
			t.Fatalf("%s: wrote %q", tmpl, got)
		}
	}
}

func TestExecCommandSkipsOtherActions(t *testing.T) {
	if cmd := execCommand("copy", &Context{}); cmd != nil {
		t.Fatal("non-exec action built a command")
	}
}

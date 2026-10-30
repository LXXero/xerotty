package menu

import (
	"os"
	"path/filepath"
	"testing"
)

// TestExecActionVarsAreNotShellText: the program in the terminal can
// choose every one of these values, so no quoting in them may reach the
// shell as syntax, whichever way the template writes the variable, and
// the value has to arrive whole.
func TestExecActionVarsAreNotShellText(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	touch := "touch " + marker
	value := "a b  *;" + touch + ";$(" + touch + ")`" + touch + "`';" + touch + ";'\";" + touch + ";\""

	for name, ctx := range map[string]*Context{
		"$XEROTTY_SELECTION": {Selection: value},
		"$XEROTTY_TAB_TITLE": {TabTitle: value},
		"$XEROTTY_LINK":      {Link: value},
		"$XEROTTY_CWD":       {CWD: value},
	} {
		for tmpl, want := range map[string]string{
			`"` + name + `"`:             value,
			`'` + name + `'`:             value,
			name:                         value,
			`'http://x/?q=` + name + `'`: "http://x/?q=" + value,
			`"http://x/?q=` + name + `"`: "http://x/?q=" + value,
			`'a'` + name + `"b"`:         "a" + value + "b",
		} {
			out := filepath.Join(dir, "out")
			cmd := execCommand("exec:printf %s "+tmpl+" > "+out, ctx)
			if cmd == nil {
				t.Fatal("no command")
			}
			if err := cmd.Run(); err != nil {
				t.Fatalf("%s: %v", tmpl, err)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatalf("%s: the value ran as shell code", tmpl)
			}
			got, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != want {
				t.Errorf("%s: wrote %q, want %q", tmpl, got, want)
			}
		}
	}
}

// TestQuoteMenuVars: only a real use of a menu variable is rewritten.
func TestQuoteMenuVars(t *testing.T) {
	for in, want := range map[string]string{
		`$XEROTTY_LINKS`:      `$XEROTTY_LINKS`,
		`$HOME`:               `$HOME`,
		`\$XEROTTY_LINK`:      `\$XEROTTY_LINK`,
		`it\'s $XEROTTY_LINK`: `it\'s "$XEROTTY_LINK"`,
		`"a'b$XEROTTY_LINK"`:  `"a'b$XEROTTY_LINK"`,
		`'a"b$XEROTTY_LINK'`:  `'a"b'"$XEROTTY_LINK"''`,
		`trailing\`:           `trailing\`,
	} {
		if got := quoteMenuVars(in); got != want {
			t.Errorf("%s: got %s, want %s", in, got, want)
		}
	}
}

func TestExecCommandSkipsOtherActions(t *testing.T) {
	if cmd := execCommand("copy", &Context{}); cmd != nil {
		t.Fatal("non-exec action built a command")
	}
}

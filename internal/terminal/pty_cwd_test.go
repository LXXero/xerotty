package terminal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUsableCWD(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	good := t.TempDir()
	file := filepath.Join(good, "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct{ name, in, want string }{
		{"empty stays empty (inherit process cwd)", "", ""},
		{"existing dir is kept", good, good},
		{"missing dir (e.g. a remote host's path) falls back to home", filepath.Join(good, "nope", "deeper"), home},
		{"a file is not a directory", file, home},
	}
	for _, c := range cases {
		if got := usableCWD(c.in); got != c.want {
			t.Errorf("%s: usableCWD(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}

	// Home itself gone → "" so the spawn still succeeds somewhere.
	t.Setenv("HOME", filepath.Join(good, "no-home"))
	if got := usableCWD("/definitely/not/here"); got != "" {
		t.Errorf("with no usable home, got %q, want \"\"", got)
	}
}

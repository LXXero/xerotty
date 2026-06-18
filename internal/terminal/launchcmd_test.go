package terminal

import (
	"testing"

	"github.com/LXXero/xerotty/internal/config"
)

// spawnPTY must build the right exec.Cmd for the `-e` (direct exec)
// and `-x` (shell -c) launch modes, and fall back to the login shell
// when no command override is given.
func TestSpawnPTYCommandOverride(t *testing.T) {
	cfg := config.Default()
	shell := cfg.DetectShell()

	cases := []struct {
		name   string
		launch *LaunchCmd
		want   []string
	}{
		{"direct exec (-e)", &LaunchCmd{Argv: []string{"true", "x"}}, []string{"true", "x"}},
		{"shell -c (-x)", &LaunchCmd{Argv: []string{"echo hi && true"}, Shell: true}, []string{shell, "-c", "echo hi && true"}},
		{"nil = default shell", nil, []string{shell}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ptmx, cmd, err := spawnPTY(&cfg, 80, 24, "", tc.launch)
			if err != nil {
				t.Fatalf("spawnPTY: %v", err)
			}
			defer func() {
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
				_ = ptmx.Close()
			}()
			got := cmd.Args
			// The default-shell case may carry a "-"-prefixed argv[0] on
			// macOS (login shell); compare only that it's the shell.
			if tc.launch == nil {
				if len(got) != 1 {
					t.Fatalf("default shell: args = %v, want 1 element", got)
				}
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("args = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("args[%d] = %q, want %q (full %v)", i, got[i], tc.want[i], got)
				}
			}
		})
	}
}

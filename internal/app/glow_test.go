package app

import (
	"testing"

	"github.com/LXXero/xerotty/internal/config"
)

// glowIdleWakeMs is the lamp's only pacing source since the dedicated
// Go ticker was removed (wakeup-storm round 2): if it regresses to 0
// while enabled, the lamp freezes between input events; if it stops
// clamping, the idle timeout collapses and the render loop spins.
func TestGlowIdleWakeMs(t *testing.T) {
	cases := []struct {
		name    string
		enabled bool
		fps     int
		want    int
	}{
		{"disabled", false, 20, 0},
		{"default fps", true, 0, 50},   // fps<=0 defaults to 20 -> 50ms
		{"explicit 20", true, 20, 50},
		{"clamped high", true, 240, 16}, // capped at 60fps -> 16ms
		{"low fps", true, 4, 250},
	}
	for _, c := range cases {
		a := &App{cfg: config.Config{}}
		a.cfg.Appearance.Glow.Enabled = c.enabled
		a.cfg.Appearance.Glow.FPS = c.fps
		if got := a.glowIdleWakeMs(); got != c.want {
			t.Errorf("%s: glowIdleWakeMs() = %d, want %d", c.name, got, c.want)
		}
	}
}

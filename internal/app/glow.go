// Lava-lamp background glow: slow-drifting soft color blobs behind
// the terminal cells. The CSS-world equivalent is blurred clip-path
// polygons (https://andrewwalpole.com/blog/glowing-blurred-backgrounds-with-css/);
// in our ImGui/GL world a heavily-blurred polygon IS a radial
// gradient splat, so we skip the blur entirely: one precomputed
// soft-falloff disc texture, a handful of big tinted quads drifting
// on lissajous paths, drawn under the cell pass. Default-background
// cells don't paint (renderer skips them), so the glow shows through
// everywhere except explicitly-colored cell runs — which read as
// floating on top, exactly the effect wanted.
//
// Power note: animation means self-waking an otherwise event-driven
// render loop, so the feature is opt-in, ticks at a configurable low
// fps, and the ticker stops the moment it's disabled.

package app

import (
	"image"
	"math"
	"time"

	"github.com/AllenDang/cimgui-go/imgui"

	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/platform"
	"github.com/LXXero/xerotty/internal/renderer"
)

// glowTexSize is the soft-disc texture's edge. 256 is plenty: it's
// only ever magnified, and the falloff is what matters.
const glowTexSize = 256

var (
	glowTex   imgui.TextureRef
	glowTexOK bool
	// Animation clock: advances only while animating. Pausing the
	// wake ticker alone is NOT enough to freeze the lamp — frames
	// triggered by unrelated events (shell output, daemon frames)
	// would sample wall-clock time and make the blobs lurch between
	// positions. Freezing the clock makes those frames render
	// byte-identical, motionless goo.
	glowClock    float64
	glowLastNano int64
)

// glowTime advances and returns the animation clock. Only the main
// render goroutine calls this (one frame at a time), so plain vars
// are fine.
func glowTime(animate bool) float64 {
	now := time.Now().UnixNano()
	if glowLastNano == 0 {
		glowLastNano = now
	}
	if animate {
		glowClock += float64(now-glowLastNano) / 1e9
	}
	glowLastNano = now
	return glowClock
}

// glowTexture lazily builds the soft radial-falloff disc: white RGB,
// alpha = (1-r²)² — a Wendland-style kernel that's smooth at both
// the center and the rim (no visible edge ring). Tinting happens at
// draw time via the quad color, same trick as mono glyph masks.
func glowTexture() (imgui.TextureRef, bool) {
	if glowTexOK {
		return glowTex, true
	}
	img := image.NewRGBA(image.Rect(0, 0, glowTexSize, glowTexSize))
	c := float64(glowTexSize-1) / 2
	for y := 0; y < glowTexSize; y++ {
		for x := 0; x < glowTexSize; x++ {
			dx := (float64(x) - c) / c
			dy := (float64(y) - c) / c
			r2 := dx*dx + dy*dy
			var a float64
			if r2 < 1 {
				s := 1 - r2
				a = s * s
			}
			i := (y*glowTexSize + x) * 4
			img.Pix[i+0] = 0xFF
			img.Pix[i+1] = 0xFF
			img.Pix[i+2] = 0xFF
			img.Pix[i+3] = uint8(a * 255)
		}
	}
	glowTex = platform.Textures().CreateTextureRgba(img, glowTexSize, glowTexSize)
	glowTexOK = true
	return glowTex, true
}

// glowColors picks the blob palette: explicit config hex colors, or
// theme-derived accents (blue/magenta/cyan/green — dracula glows
// purple, gruvbox glows amber, for free).
func glowColors(cfg *config.GlowConfig, theme *renderer.Theme) []uint32 {
	if len(cfg.Colors) > 0 {
		out := make([]uint32, 0, len(cfg.Colors))
		for _, h := range cfg.Colors {
			out = append(out, renderer.HexToABGR(h))
		}
		return out
	}
	return []uint32{theme.ANSI[4], theme.ANSI[5], theme.ANSI[6], theme.ANSI[2]}
}

// drawGlow paints the blob layer into dl, covering the window rect
// (origin..origin+size, drawlist space). Deterministic from time —
// no per-window animation state, so every window shares the lamp.
func drawGlow(dl *imgui.DrawList, originX, originY, width, height float32, theme *renderer.Theme, cfg *config.GlowConfig, animate bool) {
	if width <= 0 || height <= 0 {
		return
	}
	tex, ok := glowTexture()
	if !ok {
		return
	}
	blobs := cfg.Blobs
	if blobs <= 0 {
		blobs = 5
	}
	if blobs > 16 {
		blobs = 16
	}
	speed := cfg.Speed
	if speed <= 0 {
		speed = 1
	}
	scale := float32(cfg.Scale)
	if scale <= 0 {
		scale = 0.7
	}
	intensity := cfg.Intensity
	if intensity <= 0 {
		intensity = 0.35
	}
	if intensity > 1 {
		intensity = 1
	}
	palette := glowColors(cfg, theme)
	t := glowTime(animate) * speed

	minV := imgui.Vec2{X: originX, Y: originY}
	maxV := imgui.Vec2{X: originX + width, Y: originY + height}
	dl.PushClipRectV(minV, maxV, false)
	defer dl.PopClipRect()

	// splat draws one soft elliptical gradient. Overlapping splats
	// SUM alpha where they intersect — that brighter shared region
	// is what sells the "one connected mass" illusion below.
	splat := func(cx, cy, rx, ry float64, col uint32) {
		dl.AddImageV(
			tex,
			imgui.Vec2{X: float32(cx - rx), Y: float32(cy - ry)},
			imgui.Vec2{X: float32(cx + rx), Y: float32(cy + ry)},
			imgui.Vec2{X: 0, Y: 0}, imgui.Vec2{X: 1, Y: 1},
			col,
		)
	}
	base := float64(max32(width, height))
	for i := 0; i < blobs; i++ {
		fi := float64(i)
		// Golden-ratio phase spreading keeps blobs from ever
		// synchronizing; incommensurate frequencies keep the path
		// from looping visibly.
		p1 := fi * 2.39996
		p2 := fi*1.7 + 1.3
		p3 := fi*0.9 + 4.1

		// Lava drift: mostly VERTICAL travel with a lazy horizontal
		// wander — lamps convect up and down, they don't orbit.
		cx := float64(originX) + float64(width)*(0.5+0.36*math.Sin(t*0.043+p1)*math.Cos(t*0.029+p2))
		cy := float64(originY) + float64(height)*(0.5+0.46*math.Sin(t*0.067+p2))
		rad := float64(scale) * base * (0.26 + 0.08*math.Sin(t*0.083+p3))

		// The pinch cycle: sep runs 0→1→0 on a slow per-blob period.
		//   sep≈0   — lobes coincide: one fat blob
		//   sep mid — overlapping gradients form the peanut/neck
		//   sep≈1   — the small lobe has pinched off and drifts free
		// sin² gives long dwell at the extremes (blobby most of the
		// time, the dramatic split is the event, not the steady state).
		sepPhase := math.Sin(t*0.047 + p3)
		sep := sepPhase * sepPhase
		gap := rad * 1.9 * sep

		// Pinch direction: mostly vertical (the daughter blob rises
		// away), wobbling a little so splits aren't all parallel.
		ang := math.Pi/2 + 0.5*math.Sin(t*0.031+p2)
		dx, dy := math.Cos(ang), -math.Sin(ang)

		// Vertical stretch grows as the blob pinches — taffy physics.
		stretch := 1 + 0.45*sep

		a := intensity * (0.75 + 0.25*math.Sin(t*0.11+p3))
		rgb := palette[i%len(palette)] & 0x00FFFFFF
		colA := rgb | uint32(a*255)<<24
		colB := rgb | uint32(a*0.9*255)<<24

		// Parent lobe: thins slightly as it donates mass.
		rA := rad * (1 - 0.22*sep)
		splat(cx-dx*gap*0.35, cy-dy*gap*0.35, rA, rA*stretch, colA)
		// Daughter lobe: smaller, breathing, rides the far end of
		// the neck and ends up a free-floating blob at full sep.
		rB := rad * (0.55 + 0.08*math.Sin(t*0.097+p1))
		splat(cx+dx*gap*0.65, cy+dy*gap*0.65, rB, rB*(1+0.3*sep), colB)
		// Neck filler: a faint splat at the midpoint that fades out
		// exactly as the pinch completes — the membrane that snaps.
		if sep > 0.05 && sep < 0.92 {
			neck := (1 - sep) * 0.5
			colN := rgb | uint32(a*neck*255)<<24
			rN := rad * 0.45
			splat(cx+dx*gap*0.15, cy+dy*gap*0.15, rN, rN*1.2, colN)
		}
	}
}

func max32(a, b float32) float32 {
	if a > b {
		return a
	}
	return b
}

// ensureGlowTicker starts/stops the low-fps self-wake that drives
// the animation. The render loop is otherwise event-driven (and we
// want to keep it that way for idle power), so an animated backdrop
// has to bring its own heartbeat. Called at startup and whenever
// prefs apply.
func (a *App) ensureGlowTicker() {
	enabled := a.cfg.Appearance.Glow.Enabled
	a.glowPauseUnfocused.Store(a.cfg.Appearance.Glow.PauseUnfocused)
	if enabled && a.glowStop == nil {
		fps := a.cfg.Appearance.Glow.FPS
		if fps <= 0 {
			fps = 20
		}
		if fps > 60 {
			fps = 60
		}
		stop := make(chan struct{})
		a.glowStop = stop
		a.glowFocusActive.Store(true)
		go func() {
			tick := time.NewTicker(time.Second / time.Duration(fps))
			defer tick.Stop()
			for {
				select {
				case <-stop:
					return
				case <-tick.C:
					// Don't burn frames while no xerotty window has
					// focus (unless configured to) — the focus-change
					// event itself runs a frame on return.
					if !a.glowPauseUnfocused.Load() || a.glowFocusActive.Load() {
						platform.PostWake()
					}
				}
			}
		}()
	} else if !enabled && a.glowStop != nil {
		close(a.glowStop)
		a.glowStop = nil
	}
}

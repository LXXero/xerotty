package renderer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/AllenDang/cimgui-go/imgui"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/dpi"
	"github.com/LXXero/xerotty/internal/fontsys"
)

// PixelSize converts a config Font.Size (in points, matching the convention
// of xfce4-terminal/gnome-terminal/Pango) into the pixel size that ImGui's
// font atlas needs. Falls back to 14pt when unset.
func PixelSize(cfg *config.Config) float32 {
	pt := cfg.Font.Size
	if pt <= 0 {
		pt = 14
	}
	return dpi.PointsToPixels(pt)
}

// CellMetrics holds the pixel dimensions of a single terminal cell.
type CellMetrics struct {
	Width  float32
	Height float32
}

// terminalGlyphRanges builds glyph ranges for terminal rendering. Beyond the
// default Latin-1 set this covers everything a modern shell, prompt theme,
// or TUI is likely to emit: extended Latin, arrows, math, box drawing, block
// elements, dingbats, and the full Private Use Area where Nerd Fonts live.
// ImGui only rasterizes glyphs the loaded font actually contains, so wide
// ranges are nearly free for fonts without those glyphs.
// The returned GlyphRange must not be freed until the font atlas is built.
var termGlyphRanges imgui.GlyphRange

func terminalGlyphRanges() *imgui.Wchar {
	if termGlyphRanges == 0 {
		builder := imgui.NewFontGlyphRangesBuilder()
		builder.AddRanges(imgui.CurrentIO().Fonts().GlyphRangesDefault())
		addRange := func(lo, hi imgui.Wchar) {
			for ch := lo; ch <= hi; ch++ {
				builder.AddChar(ch)
			}
		}
		// Latin Extended-A/B (European diacritics)
		addRange(0x0100, 0x024F)
		// General Punctuation (en/em dashes, fancy quotes, ellipsis, NBSP variants)
		addRange(0x2000, 0x206F)
		// Superscripts/Subscripts
		addRange(0x2070, 0x209F)
		// Letterlike Symbols (™ ℃ № ℅), Number Forms, Arrows, Math Operators,
		// Misc Technical (⌘ ⌥ ⌃ ⏎ ⇧ keyboard symbols)
		addRange(0x2100, 0x23FF)
		// Box Drawing, Block Elements, Geometric Shapes, Misc Symbols, Dingbats
		addRange(0x2500, 0x27BF)
		// Misc Symbols and Arrows (↻ ⬆ ⭐ etc.)
		addRange(0x2B00, 0x2BFF)
		// Private Use Area — Nerd Fonts, Powerline (full extended set), Pomicons,
		// Font Awesome, Devicons, Codicons, Octicons, Material Design, etc.
		addRange(0xE000, 0xF8FF)
		termGlyphRanges = imgui.NewGlyphRange()
		builder.BuildRanges(termGlyphRanges)
	}
	return termGlyphRanges.Data()
}

// LoadFont loads the configured font (regular + bold variant if available)
// into the ImGui font atlas. Call MeasureCell after the first frame to get
// accurate metrics. Bold may be nil if no matching bold face was discovered.
func LoadFont(cfg *config.Config) (regular, bold *imgui.Font) {
	regular, bold, _ = loadFontResolved(cfg)
	return
}

// ReloadFont clears the atlas and loads the configured font afresh.
// Returns the new fonts and the regular path actually used (for status
// display). Must be called from a BeforeRender hook (i.e. before the
// frame's NewFrame), never mid-frame — ImGui's per-frame state
// snapshots the current font during NewFrame and freeing that font
// later in the same frame leaves a dangling pointer that crashes
// during Render. The dynamic atlas (RendererHasTextures) rebuilds
// itself lazily on the first font access in NewFrame, so no explicit
// Build() call is needed (and would assert if attempted).
func ReloadFont(cfg *config.Config) (regular, bold *imgui.Font, path string) {
	io := imgui.CurrentIO()
	io.Fonts().Clear()
	return loadFontResolved(cfg)
}

// ResolveFontPath returns the path that LoadFont would actually use, without
// touching the atlas. Useful for showing a "→ resolved to ..." preview in the
// preferences dialog.
func ResolveFontPath(cfg *config.Config) string {
	if cfg.Font.Path != "" {
		if _, err := os.Stat(cfg.Font.Path); err == nil {
			return cfg.Font.Path
		}
	}
	family := cfg.Font.Family
	if family == "" {
		family = "monospace"
	}
	return findFont(family)
}

func loadFontResolved(cfg *config.Config) (*imgui.Font, *imgui.Font, string) {
	io := imgui.CurrentIO()
	pt := cfg.Font.Size
	if pt <= 0 {
		pt = 14
	}
	pxSize := dpi.PointsToPixels(pt)

	ranges := terminalGlyphRanges()
	var font *imgui.Font
	var loadedPath string

	// terminalFontConfig returns a FontConfig tuned for terminal rendering:
	// - PixelSnapH/V: snap each glyph to integer pixel positions, preventing
	//   anti-aliasing drift between rows that breaks half-block (▀ ▄) and
	//   line-drawing (─ │) character continuity
	// - OversampleH/V = 1: no sub-pixel rasterization (default 3 produces
	//   crisper text at fractional positions but bakes sub-pixel AA into the
	//   atlas, which then misaligns when stacked at integer cell offsets)
	terminalFontConfig := func() *imgui.FontConfig {
		fc := imgui.NewFontConfig()
		fc.SetPixelSnapH(true)
		fc.SetPixelSnapV(true)
		fc.SetOversampleH(1)
		fc.SetOversampleV(1)
		return fc
	}

	// safeToLoad rejects fonts that would crash ImGui's bundled stbtt parser.
	// Currently only filters variable fonts (fvar/gvar tables) — the parser
	// asserts and the assert is caught by cimgui-go's Go-level handler that
	// re-panics, which we cannot recover from.
	safeToLoad := func(path string) bool {
		if fontsys.IsVariableFont(path) {
			fmt.Fprintf(os.Stderr, "xerotty: %s is a variable font; ImGui's stbtt can't parse it, skipping\n", path)
			return false
		}
		return true
	}

	if cfg.Font.Path != "" {
		if _, err := os.Stat(cfg.Font.Path); err == nil && safeToLoad(cfg.Font.Path) {
			fc := terminalFontConfig()
			font = io.Fonts().AddFontFromFileTTFV(cfg.Font.Path, pxSize, fc, ranges)
			loadedPath = cfg.Font.Path
		}
	}

	if font == nil {
		family := cfg.Font.Family
		if family == "" {
			family = "monospace"
		}
		path := findFont(family)
		if path != "" && safeToLoad(path) {
			fmt.Fprintf(os.Stderr, "xerotty: loading font %s at %.1fpx (%.0fpt @ %.0fdpi)\n",
				path, pxSize, pt, dpi.Display())
			fc := terminalFontConfig()
			font = io.Fonts().AddFontFromFileTTFV(path, pxSize, fc, ranges)
			loadedPath = path
		} else if path == "" {
			fmt.Fprintf(os.Stderr, "xerotty: font family %q not found\n", family)
		}
	}

	if font == nil {
		fmt.Fprintln(os.Stderr, "xerotty: no font found, using ImGui default")
		font = io.Fonts().AddFontDefault()
	}

	// Terminal cells go through the OS-backed glyph cache which uses
	// CoreText's per-codepoint cascade. Tab labels and other ImGui UI
	// text still render through this static atlas though, so we merge
	// in a small set of fallback fonts that cover common symbols
	// (window-title sigils like ✳ U+2733, status indicators, etc.)
	// the user's primary monospace might lack. Cell metrics are driven
	// by the glyph cache's primary-only LineMetrics, so taller merge
	// fonts don't inflate row height in the terminal grid.
	if loadedPath != "" {
		for _, mergePath := range findUIFallbacks() {
			mc := terminalFontConfig()
			mc.SetMergeMode(true)
			io.Fonts().AddFontFromFileTTFV(mergePath, pxSize, mc, ranges)
		}
	}

	var bold *imgui.Font
	if loadedPath != "" {
		if boldPath := findBoldVariant(loadedPath); boldPath != "" {
			fmt.Fprintf(os.Stderr, "xerotty: loading bold font %s\n", boldPath)
			fc := terminalFontConfig()
			bold = io.Fonts().AddFontFromFileTTFV(boldPath, pxSize, fc, ranges)
		}
	}

	return font, bold, loadedPath
}

// findUIFallbacks returns font paths to merge into ImGui's static atlas
// so tab labels / dialog text can render symbols the user's primary
// monospace lacks (e.g. Monaco doesn't have ✳ U+2733 which appears in
// claude's window title). Picked for broad symbol coverage with metrics
// close to a typical monospace font; cell rendering is unaffected
// because terminal cells use the glyph cache, not this atlas.
func findUIFallbacks() []string {
	candidates := []string{
		// macOS — Menlo carries most Misc Symbols, Dingbats, Arrows.
		"/System/Library/Fonts/Menlo.ttc",
		// Linux — DejaVu Sans is a similarly broad fallback.
		"/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
		"/usr/share/fonts/dejavu/DejaVuSans.ttf",
	}
	var found []string
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			found = append(found, p)
		}
	}
	return found
}

// findBoldVariant looks for a Bold font file sitting next to regularPath,
// trying common naming conventions (Foo-Regular → Foo-Bold, Foo → Foo-Bold,
// FooRegular → FooBold). Returns "" if none found.
func findBoldVariant(regularPath string) string {
	dir := filepath.Dir(regularPath)
	base := filepath.Base(regularPath)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)

	var candidates []string

	// Foo-Regular / Foo_Regular → Foo-Bold / Foo_Bold (preserve separator)
	for _, sep := range []string{"-", "_"} {
		suf := sep + "Regular"
		if strings.HasSuffix(stem, suf) {
			prefix := strings.TrimSuffix(stem, suf)
			candidates = append(candidates,
				prefix+sep+"Bold"+ext,
				prefix+sep+"bold"+ext,
			)
		}
	}

	// Glued "FooRegular" → "FooBold"
	if strings.HasSuffix(stem, "Regular") {
		prefix := strings.TrimSuffix(stem, "Regular")
		candidates = append(candidates, prefix+"Bold"+ext)
	}

	// Bare "Foo" → "Foo-Bold" / "FooBold"
	candidates = append(candidates,
		stem+"-Bold"+ext,
		stem+"_Bold"+ext,
		stem+"Bold"+ext,
	)

	for _, c := range candidates {
		p := filepath.Join(dir, c)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// MeasureCell measures the actual cell dimensions of the loaded font.
// Must be called after the font atlas has been built (i.e. after first frame).
func MeasureCell() CellMetrics {
	size := imgui.CalcTextSizeV("M", false, 0)
	h := imgui.FontSize() * 1.0
	if size.Y > h {
		h = size.Y
	}
	return CellMetrics{Width: size.X, Height: h}
}

// findFont searches common font directories for a font matching the family name.
func findFont(family string) string {
	// Handle generic "monospace" alias. macOS ships Menlo/SF Mono — list them
	// first so Mac users get a sensible default before the Linux fallbacks.
	monospaceDefaults := []string{
		"Menlo",
		"SFNSMono",
		"JetBrainsMono",
		"DejaVuSansMono",
		"LiberationMono",
		"UbuntuMono",
		"Hack",
		"FiraCode",
		"SourceCodePro",
		"Inconsolata",
	}

	families := []string{family}
	if strings.EqualFold(family, "monospace") {
		families = monospaceDefaults
	}

	dirs := []string{
		"/usr/share/fonts",
		"/usr/local/share/fonts",
		"/System/Library/Fonts",
		"/System/Library/Fonts/Supplemental",
		"/Library/Fonts",
	}

	home, err := os.UserHomeDir()
	if err == nil {
		dirs = append(dirs, filepath.Join(home, ".local", "share", "fonts"))
		dirs = append(dirs, filepath.Join(home, ".fonts"))
		dirs = append(dirs, filepath.Join(home, "Library", "Fonts"))
	}

	for _, fam := range families {
		// Common naming patterns (.ttc is macOS TrueType collections —
		// Menlo, Courier, etc. ship as a single .ttc with all weights)
		patterns := []string{
			fam + "-Regular.ttf",
			fam + "-Regular.otf",
			fam + "Regular.ttf",
			fam + ".ttf",
			fam + ".otf",
			fam + ".ttc",
		}

		for _, dir := range dirs {
			for _, pat := range patterns {
				// Check in subdirectories (e.g. /usr/share/fonts/TTF/)
				matches, _ := filepath.Glob(filepath.Join(dir, "*", pat))
				if len(matches) > 0 {
					return matches[0]
				}
				// Check direct children
				direct := filepath.Join(dir, pat)
				if _, err := os.Stat(direct); err == nil {
					return direct
				}
			}
		}
	}

	return ""
}

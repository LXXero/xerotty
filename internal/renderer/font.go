package renderer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/AllenDang/cimgui-go/imgui"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/dpi"
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

// terminalGlyphRanges builds glyph ranges for terminal rendering:
// ASCII + Latin-1 Supplement + box drawing + block elements + misc symbols.
// The returned GlyphRange must not be freed until the font atlas is built.
var termGlyphRanges imgui.GlyphRange

func terminalGlyphRanges() *imgui.Wchar {
	if termGlyphRanges == 0 {
		builder := imgui.NewFontGlyphRangesBuilder()
		builder.AddRanges(imgui.CurrentIO().Fonts().GlyphRangesDefault())
		// Box Drawing: U+2500–U+257F
		// Block Elements: U+2580–U+259F
		// Geometric Shapes: U+25A0–U+25FF (used in some TUIs)
		// Misc Symbols: U+2600–U+26FF
		// Powerline: U+E0A0–U+E0B3
		for ch := imgui.Wchar(0x2500); ch <= 0x259F; ch++ {
			builder.AddChar(ch)
		}
		for ch := imgui.Wchar(0x25A0); ch <= 0x25FF; ch++ {
			builder.AddChar(ch)
		}
		for ch := imgui.Wchar(0xE0A0); ch <= 0xE0B3; ch++ {
			builder.AddChar(ch)
		}
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
// Returns the new fonts and the regular path actually used (for status display).
// Must be called between frames; the renderer will rebuild textures on demand
// (ImGui 1.92+ with RendererHasTextures) before the next draw.
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

	if cfg.Font.Path != "" {
		if _, err := os.Stat(cfg.Font.Path); err == nil {
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
		if path != "" {
			fmt.Fprintf(os.Stderr, "xerotty: loading font %s at %.1fpx (%.0fpt @ %.0fdpi)\n",
				path, pxSize, pt, dpi.Display())
			fc := terminalFontConfig()
			font = io.Fonts().AddFontFromFileTTFV(path, pxSize, fc, ranges)
			loadedPath = path
		} else {
			fmt.Fprintf(os.Stderr, "xerotty: font family %q not found\n", family)
		}
	}

	if font == nil {
		fmt.Fprintln(os.Stderr, "xerotty: no font found, using ImGui default")
		font = io.Fonts().AddFontDefault()
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
	// Handle generic "monospace" alias
	monospaceDefaults := []string{
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
		"/Library/Fonts",
	}

	home, err := os.UserHomeDir()
	if err == nil {
		dirs = append(dirs, filepath.Join(home, ".local", "share", "fonts"))
		dirs = append(dirs, filepath.Join(home, ".fonts"))
		dirs = append(dirs, filepath.Join(home, "Library", "Fonts"))
	}

	for _, fam := range families {
		// Common naming patterns
		patterns := []string{
			fam + "-Regular.ttf",
			fam + "-Regular.otf",
			fam + "Regular.ttf",
			fam + ".ttf",
			fam + ".otf",
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

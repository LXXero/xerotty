package renderer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/AllenDang/cimgui-go/imgui"
	"github.com/LXXero/xerotty/internal/config"
)

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

// LoadFont loads the configured font into the ImGui font atlas.
// Call MeasureCell after the first frame to get accurate metrics.
func LoadFont(cfg *config.Config) *imgui.Font {
	io := imgui.CurrentIO()
	fontSize := cfg.Font.Size
	if fontSize <= 0 {
		fontSize = 14
	}

	ranges := terminalGlyphRanges()
	var font *imgui.Font

	// Try explicit path first
	if cfg.Font.Path != "" {
		if _, err := os.Stat(cfg.Font.Path); err == nil {
			fc := imgui.NewFontConfig()
			font = io.Fonts().AddFontFromFileTTFV(cfg.Font.Path, fontSize, fc, ranges)
		}
	}

	// Try finding font by family name in common directories
	if font == nil {
		family := cfg.Font.Family
		if family == "" {
			family = "monospace"
		}
		path := findFont(family)
		if path != "" {
			fmt.Fprintf(os.Stderr, "xerotty: loading font %s at %.0fpt\n", path, fontSize)
			fc := imgui.NewFontConfig()
			font = io.Fonts().AddFontFromFileTTFV(path, fontSize, fc, ranges)
		} else {
			fmt.Fprintf(os.Stderr, "xerotty: font family %q not found\n", family)
		}
	}

	// Fallback to ImGui default
	if font == nil {
		fmt.Fprintln(os.Stderr, "xerotty: no font found, using ImGui default")
		font = io.Fonts().AddFontDefault()
	}

	return font
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
	}

	home, err := os.UserHomeDir()
	if err == nil {
		dirs = append(dirs, filepath.Join(home, ".local", "share", "fonts"))
		dirs = append(dirs, filepath.Join(home, ".fonts"))
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

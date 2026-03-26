package renderer

import (
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

// LoadFont loads the configured font into the ImGui font atlas.
// Call MeasureCell after the first frame to get accurate metrics.
func LoadFont(cfg *config.Config) *imgui.Font {
	io := imgui.CurrentIO()
	fontSize := cfg.Font.Size
	if fontSize <= 0 {
		fontSize = 14
	}

	var font *imgui.Font

	// Try explicit path first
	if cfg.Font.Path != "" {
		if _, err := os.Stat(cfg.Font.Path); err == nil {
			fc := imgui.NewFontConfig()
			font = io.Fonts().AddFontFromFileTTFV(cfg.Font.Path, fontSize, fc, nil)
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
			fc := imgui.NewFontConfig()
			font = io.Fonts().AddFontFromFileTTFV(path, fontSize, fc, nil)
		}
	}

	// Fallback to ImGui default
	if font == nil {
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

package renderer

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"unicode"
)

// FontEntry is a discovered font face on disk.
type FontEntry struct {
	Family string // human-readable family name, e.g. "DejaVu Sans Mono"
	Path   string // absolute path to the .ttf/.otf file
}

// DiscoverMonospaceFonts walks system font directories on the current OS
// and returns deduplicated regular-weight monospace fonts, sorted by family.
// Filename heuristics are used — they are imperfect but require no native deps.
func DiscoverMonospaceFonts() []FontEntry {
	all := scanAllFonts()
	seen := map[string]bool{}
	var out []FontEntry
	for _, e := range all {
		if !looksMonospace(e.Family) {
			continue
		}
		key := strings.ToLower(e.Family)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Family) < strings.ToLower(out[j].Family)
	})
	return out
}

// DiscoverAllFonts is the unfiltered version — useful when the user wants a
// non-monospace font, or when our heuristic misses an exotic family.
func DiscoverAllFonts() []FontEntry {
	all := scanAllFonts()
	seen := map[string]bool{}
	var out []FontEntry
	for _, e := range all {
		key := strings.ToLower(e.Family)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Family) < strings.ToLower(out[j].Family)
	})
	return out
}

func scanAllFonts() []FontEntry {
	var out []FontEntry
	for _, dir := range fontDirs() {
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				return nil
			}
			ext := strings.ToLower(filepath.Ext(path))
			if ext != ".ttf" && ext != ".otf" {
				return nil
			}
			base := filepath.Base(path)
			if !isRegularWeight(base) {
				return nil
			}
			fam := familyFromFilename(base)
			if fam == "" {
				return nil
			}
			out = append(out, FontEntry{Family: fam, Path: path})
			return nil
		})
	}
	return out
}

func fontDirs() []string {
	var dirs []string
	switch runtime.GOOS {
	case "darwin":
		dirs = []string{"/System/Library/Fonts", "/Library/Fonts"}
	default: // linux, bsd, etc.
		dirs = []string{"/usr/share/fonts", "/usr/local/share/fonts"}
	}
	if home, err := os.UserHomeDir(); err == nil {
		switch runtime.GOOS {
		case "darwin":
			dirs = append(dirs, filepath.Join(home, "Library/Fonts"))
		default:
			dirs = append(dirs,
				filepath.Join(home, ".local/share/fonts"),
				filepath.Join(home, ".fonts"),
			)
		}
	}
	return dirs
}

// styleSuffixes are stripped from filenames to recover the family name.
// Order matters: longer suffixes must come first to avoid partial matches.
var styleSuffixes = []string{
	"BoldItalic", "BoldOblique", "MediumItalic", "LightItalic",
	"ThinItalic", "BlackItalic", "HeavyItalic", "BookItalic",
	"SemiBoldItalic", "ExtraBoldItalic", "ExtraLightItalic",
	"DemiBoldItalic", "BoldIt", "MediumIt", "LightIt", "BlackIt",
	"ExtraLightIt", "ExtraBoldIt", "SemiBoldIt", "DemiBoldIt",
	"ExtraBold", "SemiBold", "ExtraLight", "DemiBold", "UltraLight",
	"Regular", "Medium", "Italic", "Oblique", "Bold",
	"Light", "Thin", "Black", "Heavy", "Book", "Roman",
	"Obl", "It",
}

// nonRegularKeywords are case-insensitive substrings that, when present in
// the *style portion* of a filename, indicate it's not a regular weight.
var nonRegularKeywords = []string{
	"bold", "italic", "oblique", "light", "thin", "black", "heavy",
	"medium", "book", "semibold", "extralight", "extrabold", "demibold",
}

// isRegularWeight returns false for filenames whose style suffix indicates
// bold, italic, light, etc. — we only want one entry per family in the picker.
// We look at the portion after the last "-" or "_" so that "JetBrainsMono" is
// kept while "JetBrainsMono-Bold" and "JetBrainsMono-It" are both rejected.
func isRegularWeight(filename string) bool {
	base := strings.TrimSuffix(filename, filepath.Ext(filename))
	style := base
	for _, sep := range []string{"-", "_"} {
		if i := strings.LastIndex(base, sep); i >= 0 {
			style = base[i+1:]
			break
		}
	}
	lower := strings.ToLower(style)
	for _, kw := range nonRegularKeywords {
		if strings.Contains(lower, kw) {
			return false
		}
	}
	// Bare "It" / "Obl" suffix on its own is italic/oblique
	if lower == "it" || lower == "obl" {
		return false
	}
	return true
}

// familyFromFilename derives a display family name from a font filename.
//
//	"DejaVuSansMono-Regular.ttf" → "DejaVu Sans Mono"
//	"FiraCode-Regular.otf"       → "Fira Code"
//	"JetBrainsMono.ttf"          → "JetBrains Mono"
func familyFromFilename(name string) string {
	base := strings.TrimSuffix(name, filepath.Ext(name))

	// Strip a "-Style" or "_Style" suffix if present.
	for _, sep := range []string{"-", "_"} {
		if i := strings.LastIndex(base, sep); i >= 0 {
			suf := base[i+1:]
			for _, known := range styleSuffixes {
				if strings.EqualFold(suf, known) {
					base = base[:i]
					break
				}
			}
		}
	}

	// Strip a glued "Style" suffix (e.g. "FiraCodeRegular") if there's still
	// a font name left after removing it.
	for _, suf := range styleSuffixes {
		if strings.HasSuffix(base, suf) && len(base) > len(suf)+2 {
			base = strings.TrimSuffix(base, suf)
			break
		}
	}

	base = strings.TrimRight(base, "-_ ")
	return splitCamelCase(base)
}

// splitCamelCase inserts spaces at lower→upper boundaries:
// "DejaVuSansMono" → "DejaVu Sans Mono".
func splitCamelCase(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	runes := []rune(s)
	for i, r := range runes {
		if i > 0 && unicode.IsUpper(r) {
			prev := runes[i-1]
			next := rune(0)
			if i+1 < len(runes) {
				next = runes[i+1]
			}
			if unicode.IsLower(prev) || (next != 0 && unicode.IsLower(next)) {
				b.WriteByte(' ')
			}
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// monospaceKeywords are case-insensitive substrings that mark a font as
// (probably) monospaced. Filename-only — no glyph metrics inspection.
var monospaceKeywords = []string{
	"mono", "code", "console", "fixed", "term",
	"hack", "fira code", "jetbrains", "source code",
	"inconsolata", "cascadia", "menlo", "monaco", "iosevka",
	"anonymous", "courier", "ubuntu mono", "noto sans mono",
	"liberation mono", "roboto mono", "space mono", "victor mono",
	"go mono",
}

func looksMonospace(family string) bool {
	lower := strings.ToLower(family)
	for _, kw := range monospaceKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

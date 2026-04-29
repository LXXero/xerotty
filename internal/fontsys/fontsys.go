// Package fontsys is xerotty's interface to the OS font services. Unlike
// ImGui's static font atlas, it lets us discover at runtime which font
// has a glyph for a given codepoint, enumerate installed fonts for the
// preferences picker, and rasterize glyphs into our own glyph cache so
// emoji, Nerd Font icons, and other glyphs the primary terminal font
// lacks render correctly.
//
// macOS uses CoreText. Linux uses fontconfig + FreeType. Each platform
// has a build-tagged implementation of the System interface.
package fontsys

// FontInfo identifies a font without opening it. Path is the on-disk
// location; on macOS this comes from CTFontCopyAttribute(kCTFontURL...).
// On Linux it's resolved via FcPatternGetString(FC_FILE).
type FontInfo struct {
	Name      string // e.g. "Menlo-Regular"
	Family    string // e.g. "Menlo"
	Style     string // e.g. "Regular", "Bold Italic"
	Path      string
	Monospace bool
}

// Glyph is a rasterized glyph ready to upload into a GL texture as an
// RGBA image. Pixels is row-major premultiplied RGBA, length =
// Width*Height*4.
//
// Bearing is the offset from the text-baseline cursor to the top-left
// of the bitmap, in pixels. Advance is the horizontal pen advance in
// pixels (typically the cell width for a monospace font).
//
// IsColor reports whether the glyph carries actual color data (e.g. an
// emoji from a color-bitmap font like Apple Color Emoji). Renderers
// should draw color glyphs with a white tint so the natural colors
// pass through, and tint monochrome glyphs with the foreground color.
type Glyph struct {
	Width, Height int
	BearingX      int
	BearingY      int
	Advance       float32
	IsColor       bool
	Pixels        []byte
}

// Font is an opened font that can rasterize glyphs and report glyph
// presence. Implementations are not goroutine-safe — callers should
// serialize access (the glyph cache does this).
type Font interface {
	// Has reports whether this font contains a glyph for r.
	Has(r rune) bool

	// Rasterize produces a grayscale glyph bitmap at pxSize. Returns
	// nil for codepoints the font doesn't have (callers should check
	// Has first to avoid an allocation).
	Rasterize(r rune, pxSize float32) (*Glyph, error)

	// LineMetrics returns the font's ascent/descent/line-height at
	// pxSize, used to align glyphs to the cell baseline.
	LineMetrics(pxSize float32) LineMetrics

	// Bold returns a bold-weight variant of this font. On macOS this
	// uses CTFontCreateCopyWithSymbolicTraits so it picks up real
	// bold faces within a .ttc collection (Menlo, Courier) and
	// synthesizes faux-bold when no real bold face exists (Monaco).
	// Returns nil if no bold representation is available — callers
	// should fall back to rendering with the regular font.
	Bold() Font

	// Close releases the underlying OS handles.
	Close()
}

// LineMetrics describes a font's vertical layout at a given pixel size.
type LineMetrics struct {
	Ascent     float32 // pixels from baseline to top of typical glyph
	Descent    float32 // pixels from baseline to bottom of typical glyph (negative or positive depending on impl)
	LineHeight float32 // total line height in pixels
}

// System is the OS-provided font discovery + loading service.
type System interface {
	// Enumerate returns every installed font. Use the Monospace flag
	// to filter for the preferences picker.
	Enumerate() ([]FontInfo, error)

	// FindForCodepoint returns a font path that has a glyph for r.
	// hint is the user's primary font; implementations may bias the
	// search toward fonts that visually match the hint's style. An
	// empty hint means "any font".
	//
	// Returns "" with no error if no installed font has the codepoint.
	FindForCodepoint(r rune, hint string) (string, error)

	// Open loads a font from disk. The returned Font must be Closed.
	Open(path string) (Font, error)
}

// Default returns the platform's default System implementation. The
// platform-specific build files set this at init time.
var Default System

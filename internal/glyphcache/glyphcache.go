// Package glyphcache rasterizes glyphs on demand via fontsys and uploads
// them as GL textures the renderer can blit through ImGui's draw list.
//
// Unlike ImGui's static font atlas (which has to be built upfront with
// every codepoint range you'll ever use, can't follow OS font fallback
// rules, and is limited to 16-bit codepoints in default builds), this
// cache:
//   - rasterizes the first time a codepoint is needed,
//   - asks the OS which font has any codepoint not in the primary font,
//   - supports the full Unicode range including supplementary plane
//     emoji as long as a font on the system can render them.
//
// Each cached glyph is its own GL texture. That trades a few extra texture
// switches per frame for skipping all the atlas bookkeeping (shelf
// packing, growing, eviction). Terminal apps emit a bounded set of
// distinct glyphs so this stays in single-digit MB of GPU memory.
package glyphcache

import (
	"github.com/AllenDang/cimgui-go/backend"
	"github.com/AllenDang/cimgui-go/imgui"
	"github.com/LXXero/xerotty/internal/fontsys"
)

// Entry is one cached glyph. The texture is the size of the rasterized
// bitmap (Width × Height). UV is implicit (0,0)-(1,1). HasTex reports
// whether Tex is a real GPU texture (zero-sized glyphs like a blank
// space have no texture but still carry a valid Advance). IsColor
// reports whether the glyph is a color emoji — renderers should draw
// color glyphs with a white (no-op) tint so the natural colors pass
// through, and monochrome glyphs with the foreground color.
type Entry struct {
	Tex      imgui.TextureRef
	HasTex   bool
	IsColor  bool
	Width    int
	Height   int
	BearingX int
	BearingY int
	Advance  float32
}

// Cache stores rasterized glyphs across a session. Lifetime is tied to a
// single primary font + pixel size; ReplaceFont rebuilds it when the
// user changes either.
//
// pxSize is the *logical* pixel size the renderer will draw at. fbScale
// is the framebuffer-to-logical ratio (2.0 on Retina, 1.0 elsewhere) —
// glyphs are rasterized at pxSize*fbScale physical pixels and the
// renderer divides bitmap dimensions by fbScale when computing on-screen
// quad coords, so the GPU sample-rate matches the physical pixel grid.
type Cache struct {
	sys     fontsys.System
	tex     backend.TextureManager
	pxSize  float32
	fbScale float32
	primary fontsys.Font
	bold    fontsys.Font // optional; nil if no bold variant on disk
	priPath string
	// fallback fonts opened on demand, keyed by path.
	fallbacks map[string]fontsys.Font
	// codepoint → fallback font path, "" means primary, "missing" means no font has it.
	resolveCache map[rune]string
	glyphs       map[glyphKey]*Entry
	missing      map[glyphKey]bool
}

// glyphKey identifies a cached glyph. Bold is treated as a separate
// glyph from regular even when the bold font ends up missing the
// codepoint and we fall back to a regular-weight rendering — that way
// repeated lookups of the same bold rune don't re-walk the fallback
// chain.
type glyphKey struct {
	r    rune
	bold bool
}

const missingSentinel = "\x00missing"

// New returns a Cache backed by sys. primaryPath is the user's preferred
// font, pxSize is the logical pixel size, fbScale is the framebuffer
// scale (2.0 on Retina, 1.0 otherwise) — pass 1.0 if you don't care
// about HiDPI sharpness. Bold variant is discovered automatically via
// fontsys.Font.Bold() (CoreText/fontconfig know about a font family's
// weights, no need for the caller to find a sibling bold file).
func New(sys fontsys.System, tex backend.TextureManager, primaryPath string, pxSize, fbScale float32) (*Cache, error) {
	primary, err := sys.Open(primaryPath)
	if err != nil {
		return nil, err
	}
	if fbScale <= 0 {
		fbScale = 1.0
	}
	return &Cache{
		sys:          sys,
		tex:          tex,
		pxSize:       pxSize,
		fbScale:      fbScale,
		primary:      primary,
		bold:         primary.Bold(),
		priPath:      primaryPath,
		fallbacks:    map[string]fontsys.Font{},
		resolveCache: map[rune]string{},
		glyphs:       map[glyphKey]*Entry{},
		missing:      map[glyphKey]bool{},
	}, nil
}

// FbScale returns the framebuffer-scale this cache was built for. The
// renderer uses this to convert glyph bitmap dimensions back to logical
// units when laying out quads.
func (c *Cache) FbScale() float32 { return c.fbScale }

// Get returns the cached entry for r, rasterizing on the first call. If
// no installed font has the codepoint, returns nil — the renderer should
// fall back to drawing a missing-glyph placeholder (we don't render one
// here so block-drawing chars handled separately can win).
//
// bold requests a bold-weight rendering. When a bold variant of the
// primary font is loaded and contains the codepoint, that's used.
// Otherwise the regular fallback chain runs (so a bold ⏵ for instance
// might come from a regular-weight symbol font — better than nothing).
func (c *Cache) Get(r rune, bold bool) *Entry {
	key := glyphKey{r: r, bold: bold}
	if e, ok := c.glyphs[key]; ok {
		return e
	}
	if c.missing[key] {
		return nil
	}
	var font fontsys.Font
	if bold && c.bold != nil && c.bold.Has(r) {
		font = c.bold
	} else {
		font = c.fontFor(r)
	}
	if font == nil {
		c.missing[key] = true
		return nil
	}
	g, err := font.Rasterize(r, c.pxSize*c.fbScale)
	if err != nil || g == nil {
		c.missing[key] = true
		return nil
	}
	e := &Entry{
		IsColor:  g.IsColor,
		Width:    g.Width,
		Height:   g.Height,
		BearingX: g.BearingX,
		BearingY: g.BearingY,
		Advance:  g.Advance,
	}
	if g.Width > 0 && g.Height > 0 {
		e.Tex = c.uploadGlyph(g)
		e.HasTex = true
	}
	c.glyphs[key] = e
	return e
}

// fontFor returns a Font that can rasterize r, opening fallbacks as
// needed. Result is cached in resolveCache so we don't re-query the OS
// for every cell that contains the same codepoint.
//
// Normally the primary font wins if it has the glyph. Exception: for
// codepoints in Unicode's emoji ranges (⚡ ❤ ⭐ etc.), we let the OS
// cascade pick first because Nerd Fonts and similar glyph-rich monospace
// fonts ship mono outline versions of these — but the user's intent
// when they show up in terminal output is almost always the colorful
// emoji presentation. Falls through to primary if the OS cascade doesn't
// hand back a color font (e.g. no color emoji font installed).
func (c *Cache) fontFor(r rune) fontsys.Font {
	emojiCandidate := isEmojiPresentationCandidate(r)
	if !emojiCandidate && c.primary != nil && c.primary.Has(r) {
		return c.primary
	}
	if path, ok := c.resolveCache[r]; ok {
		if path == "" {
			return c.primary
		}
		if path == missingSentinel {
			return nil
		}
		return c.fallbacks[path]
	}
	path, _ := c.sys.FindForCodepoint(r, c.priPath)
	if path == "" {
		// No fallback at all — fall back to primary if it has the glyph.
		if emojiCandidate && c.primary != nil && c.primary.Has(r) {
			c.resolveCache[r] = ""
			return c.primary
		}
		c.resolveCache[r] = missingSentinel
		return nil
	}
	if path == c.priPath {
		// OS handed back the primary. For emoji candidates that means
		// no color emoji font has the codepoint — primary's mono is
		// the best we can do.
		if emojiCandidate {
			c.resolveCache[r] = ""
			return c.primary
		}
		c.resolveCache[r] = missingSentinel
		return nil
	}
	font, ok := c.fallbacks[path]
	if !ok {
		f, err := c.sys.Open(path)
		if err != nil {
			c.resolveCache[r] = missingSentinel
			return nil
		}
		c.fallbacks[path] = f
		font = f
	}
	if !font.Has(r) {
		c.resolveCache[r] = missingSentinel
		return nil
	}
	c.resolveCache[r] = path
	return font
}

// uploadGlyph uploads a fontsys.Glyph's RGBA pixels into a GL texture
// and returns the ImGui TextureRef. For monochrome glyphs CoreText
// writes white into RGB and the glyph mask into alpha, so drawing with
// AddImageV(col=fg) multiplies through to the correct foreground tint.
// For color glyphs (emoji), the natural per-pixel colors are present
// in RGB and the renderer must draw with col=white to preserve them.
func (c *Cache) uploadGlyph(g *fontsys.Glyph) imgui.TextureRef {
	return c.tex.CreateTexture(unsafePtr(g.Pixels), g.Width, g.Height)
}

// isEmojiPresentationCandidate reports whether r is in a Unicode block
// where the user generally expects color emoji presentation when one is
// available. Used by fontFor to bypass the user's primary font (which
// may have a mono outline version of the codepoint via Nerd Font merges
// or similar) and prefer the OS color emoji cascade for these.
//
// Covers the major emoji blocks rather than the precise Unicode
// "Emoji_Presentation" property — overshoots by a few hundred non-emoji
// codepoints in the same blocks (geometric shapes, dingbats), which is
// fine because the OS cascade falls back to mono for those when no
// color font has them.
func isEmojiPresentationCandidate(r rune) bool {
	switch {
	case r >= 0x2600 && r <= 0x26FF: // Miscellaneous Symbols (⚡ ☂ ☀ ❤ ★ ☎ etc.)
		return true
	case r >= 0x2700 && r <= 0x27BF: // Dingbats (✂ ✈ ✉ ✏ etc.)
		return true
	case r >= 0x2B00 && r <= 0x2BFF: // Misc Symbols and Arrows (⬆ ⭐ ⚪ etc.)
		return true
	case r >= 0x1F300 && r <= 0x1F5FF: // Misc Symbols and Pictographs (🌍 🌟 🍕 etc.)
		return true
	case r >= 0x1F600 && r <= 0x1F64F: // Emoticons (😀 😂 etc.)
		return true
	case r >= 0x1F680 && r <= 0x1F6FF: // Transport and Map (🚀 🚗 etc.)
		return true
	case r >= 0x1F700 && r <= 0x1F77F: // Alchemical Symbols (mostly mono, harmless)
		return false
	case r >= 0x1F900 && r <= 0x1F9FF: // Supplemental Symbols and Pictographs (🤔 🦀 etc.)
		return true
	case r >= 0x1FA00 && r <= 0x1FAFF: // Symbols and Pictographs Extended-A (🩺 🪐 etc.)
		return true
	}
	// Variation selectors that force emoji presentation should also count
	// — but we treat them codepoint-by-codepoint above. The base emoji is
	// what matters for cascade selection.
	return false
}

// Close releases all GPU textures and OS font handles.
func (c *Cache) Close() {
	for _, e := range c.glyphs {
		if e.HasTex {
			c.tex.DeleteTexture(e.Tex)
		}
	}
	if c.primary != nil {
		c.primary.Close()
	}
	if c.bold != nil {
		c.bold.Close()
	}
	for _, f := range c.fallbacks {
		f.Close()
	}
	c.glyphs = nil
	c.fallbacks = nil
	c.resolveCache = nil
	c.missing = nil
}

// LineMetrics returns the primary font's metrics in logical pixels
// (i.e. already divided by fbScale so the renderer can use them
// directly for cell layout).
func (c *Cache) LineMetrics() fontsys.LineMetrics {
	if c.primary == nil {
		return fontsys.LineMetrics{}
	}
	m := c.primary.LineMetrics(c.pxSize * c.fbScale)
	return fontsys.LineMetrics{
		Ascent:     m.Ascent / c.fbScale,
		Descent:    m.Descent / c.fbScale,
		LineHeight: m.LineHeight / c.fbScale,
	}
}

// PrimaryAdvance returns the logical-pixel advance width of a
// representative monospace glyph from the primary font.
func (c *Cache) PrimaryAdvance() float32 {
	e := c.Get('M', false)
	if e == nil {
		return c.pxSize * 0.6
	}
	return e.Advance / c.fbScale
}

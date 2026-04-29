//go:build linux

package fontsys

/*
#cgo pkg-config: fontconfig freetype2

#include <fontconfig/fontconfig.h>
#include <ft2build.h>
#include FT_FREETYPE_H
#include FT_GLYPH_H
#include FT_BITMAP_H
#include FT_OUTLINE_H
#include <stdlib.h>
#include <stdint.h>
#include <string.h>
#include <math.h>

// Process-wide FreeType library handle. One instance is fine — FT_Face
// objects opened against it are independent.
static FT_Library xt_ft_lib = NULL;

static int xt_ft_init(void) {
    if (xt_ft_lib) return 1;
    return FT_Init_FreeType(&xt_ft_lib) == 0 ? 1 : 0;
}

// xt_fc_init lazily loads the fontconfig configuration (~/.config/fontconfig
// + system) once per process. Subsequent calls reuse it. Returns the
// FcConfig the caller should use; NULL on hard failure.
static FcConfig *xt_fc_cfg = NULL;
static FcConfig *xt_fc_init(void) {
    if (xt_fc_cfg) return xt_fc_cfg;
    if (!FcInit()) return NULL;
    xt_fc_cfg = FcConfigGetCurrent();
    return xt_fc_cfg;
}

// ---- Enumeration ----

// xt_fc_enum returns a malloc'd FcFontSet containing every installed
// SCALABLE font. Caller frees with FcFontSetDestroy. Bitmap-only formats
// (PCF, BDF, legacy X11 fonts) are filtered out at the fontconfig level
// because FreeType can technically open them but they have fixed sizes
// and look terrible at any non-native pixel size, and aren't usable as
// terminal cell fonts.
static FcFontSet *xt_fc_enum(void) {
    if (!xt_fc_init()) return NULL;
    FcPattern *pat = FcPatternCreate();
    if (!pat) return NULL;
    FcPatternAddBool(pat, FC_SCALABLE, FcTrue);
    FcObjectSet *os = FcObjectSetBuild(
        FC_FILE, FC_FAMILY, FC_FULLNAME, FC_STYLE, FC_SPACING, FC_WEIGHT,
        FC_SCALABLE, FC_OUTLINE, FC_VARIABLE,
        (char *)0);
    if (!os) {
        FcPatternDestroy(pat);
        return NULL;
    }
    FcFontSet *fs = FcFontList(NULL, pat, os);
    FcObjectSetDestroy(os);
    FcPatternDestroy(pat);
    return fs;
}

// xt_fc_is_variable reports whether a font is an OpenType variable font
// (has an fvar table). ImGui's bundled stbtt parser doesn't handle
// variable fonts cleanly and asserts on parse, so we have to keep them
// out of the static atlas pipeline. Returns 1 for VF, 0 otherwise.
static int xt_fc_is_variable(FcPattern *pat) {
    FcBool variable = FcFalse;
    if (FcPatternGetBool(pat, FC_VARIABLE, 0, &variable) == FcResultMatch && variable) {
        return 1;
    }
    return 0;
}

// xt_ft_is_variable_path checks whether the file at path is a variable
// font, by opening it with FreeType and testing FT_HAS_MULTIPLE_MASTERS.
// Defensive — used at primary-font load time to catch VFs that bypass
// the picker (e.g. a path pasted directly into the config). Returns 1
// for variable, 0 for static or unparseable.
static int xt_ft_is_variable_path(const char *path) {
    if (!xt_ft_init()) return 0;
    FT_Face face = NULL;
    if (FT_New_Face(xt_ft_lib, path, 0, &face) != 0) return 0;
    int variable = FT_HAS_MULTIPLE_MASTERS(face) ? 1 : 0;
    FT_Done_Face(face);
    return variable;
}

static int xt_fcset_count(FcFontSet *fs) { return fs ? fs->nfont : 0; }
static FcPattern *xt_fcset_at(FcFontSet *fs, int i) { return fs->fonts[i]; }
static void xt_fcset_destroy(FcFontSet *fs) { if (fs) FcFontSetDestroy(fs); }

// xt_fc_get_string copies the first matching string property out of a
// pattern as a malloc'd C string. Caller frees. NULL on miss.
static char *xt_fc_get_string(FcPattern *pat, const char *prop) {
    FcChar8 *val = NULL;
    if (FcPatternGetString(pat, prop, 0, &val) != FcResultMatch) return NULL;
    return strdup((const char *)val);
}

// xt_fc_get_int returns 0 on miss; caller treats 0 as "unknown".
static int xt_fc_get_int(FcPattern *pat, const char *prop) {
    int val = 0;
    if (FcPatternGetInteger(pat, prop, 0, &val) != FcResultMatch) return 0;
    return val;
}

// FC_SPACING values: FC_PROPORTIONAL=0, FC_DUAL=90, FC_MONO=100, FC_CHARCELL=110.
// Treat MONO and CHARCELL as monospace.
static int xt_fc_is_monospace(FcPattern *pat) {
    int spacing = xt_fc_get_int(pat, FC_SPACING);
    return (spacing == FC_MONO || spacing == FC_CHARCELL) ? 1 : 0;
}

// ---- Codepoint cascade ----

// xt_fc_match_codepoint runs a single FcFontMatch query for codepoint,
// optionally biased to color emoji fonts and/or a family hint. Returns
// the matched FcPattern (caller frees with FcPatternDestroy) or NULL.
static FcPattern *xt_fc_match_codepoint(const char *hint, FcChar32 codepoint, int prefer_color) {
    FcPattern *pat = FcPatternCreate();
    if (!pat) return NULL;

    FcCharSet *cs = FcCharSetCreate();
    if (!cs) {
        FcPatternDestroy(pat);
        return NULL;
    }
    FcCharSetAddChar(cs, codepoint);
    FcPatternAddCharSet(pat, FC_CHARSET, cs);
    FcCharSetDestroy(cs);

    // Restrict cascade fallbacks to scalable or bitmap-color fonts; PCF/BDF
    // legacy bitmaps can't render at our terminal cell sizes. NotoColorEmoji
    // is bitmap-only (CBDT) but we explicitly want it for color paths, so
    // omit the SCALABLE constraint when prefer_color is set — the renderer
    // handles bitmap-strike sizing for color fonts.
    if (!prefer_color) {
        FcPatternAddBool(pat, FC_SCALABLE, FcTrue);
    }
    if (prefer_color) {
        FcPatternAddBool(pat, FC_COLOR, FcTrue);
    }

    // Family hint biases the cascade toward visually-similar fonts —
    // useful for the mono pass (so a CJK fallback for a monospace primary
    // picks a monospace CJK font) but harmful for the color-emoji pass.
    // fontconfig prioritizes family match over color preference, so adding
    // a "JetBrains Mono" hint to a color query causes it to pick DejaVu Sans
    // (closer to the mono family) instead of NotoColorEmoji. Skip the hint
    // when we want color.
    if (hint && *hint && !prefer_color) {
        if (strchr(hint, '/')) {
            FcBlanks *blanks = FcBlanksCreate();
            int count = 0;
            FcPattern *probe = FcFreeTypeQuery((const FcChar8 *)hint, 0, blanks, &count);
            if (probe) {
                FcChar8 *fam = NULL;
                if (FcPatternGetString(probe, FC_FAMILY, 0, &fam) == FcResultMatch) {
                    FcPatternAddString(pat, FC_FAMILY, fam);
                }
                FcPatternDestroy(probe);
            }
            if (blanks) FcBlanksDestroy(blanks);
        } else {
            FcPatternAddString(pat, FC_FAMILY, (const FcChar8 *)hint);
        }
    }

    FcConfigSubstitute(NULL, pat, FcMatchPattern);
    FcDefaultSubstitute(pat);

    FcResult res;
    FcPattern *match = FcFontMatch(NULL, pat, &res);
    FcPatternDestroy(pat);
    return match;
}

// xt_fc_match_has_charset verifies the matched font actually contains
// the requested codepoint. fontconfig's substitution can hand back a
// font that doesn't satisfy the constraint when nothing matches exactly
// (e.g. asking for color when no color font has it returns the system
// default mono font). Returns 1 if the match contains codepoint.
static int xt_fc_match_has_charset(FcPattern *match, FcChar32 codepoint) {
    if (!match) return 0;
    FcCharSet *cs = NULL;
    if (FcPatternGetCharSet(match, FC_CHARSET, 0, &cs) != FcResultMatch || !cs) return 0;
    return FcCharSetHasChar(cs, codepoint) ? 1 : 0;
}

// xt_fc_find_for_codepoint asks fontconfig which installed font is the
// best fallback for a given codepoint. Two-pass strategy: first try a
// color-emoji-preferring match (so dual-presentation codepoints like
// ⚡ U+26A1 prefer the colored emoji glyph from NotoColorEmoji over the
// mono outline from DejaVu Sans). If no color font has the codepoint,
// fall back to a general (mono-allowed) match. hint is the user's
// primary font for style biasing in both passes.
static char *xt_fc_find_for_codepoint(const char *hint, FcChar32 codepoint) {
    if (!xt_fc_init()) return NULL;
    char *out = NULL;

    // Pass 1: color-emoji preferred. Only accept the match if the font
    // actually has the codepoint — fontconfig's default fallback can
    // hand back a system font that doesn't match the constraint.
    FcPattern *match = xt_fc_match_codepoint(hint, codepoint, 1);
    if (match) {
        if (xt_fc_match_has_charset(match, codepoint)) {
            FcChar8 *path = NULL;
            if (FcPatternGetString(match, FC_FILE, 0, &path) == FcResultMatch) {
                out = strdup((const char *)path);
            }
        }
        FcPatternDestroy(match);
    }
    if (out) return out;

    // Pass 2: any font (mono allowed). This is the last-resort cascade
    // for codepoints with no color emoji presentation (box-drawing,
    // mathematical symbols, CJK, etc.).
    match = xt_fc_match_codepoint(hint, codepoint, 0);
    if (!match) return NULL;
    FcChar8 *path = NULL;
    if (FcPatternGetString(match, FC_FILE, 0, &path) == FcResultMatch) {
        out = strdup((const char *)path);
    }
    FcPatternDestroy(match);
    return out;
}

// xt_fc_find_bold returns a malloc'd path to the bold-weight variant of
// the given regular font's family, or NULL if fontconfig has no separate
// bold face. Caller frees.
static char *xt_fc_find_bold(const char *regular_path) {
    if (!xt_fc_init() || !regular_path) return NULL;

    FcBlanks *blanks = FcBlanksCreate();
    int count = 0;
    FcPattern *probe = FcFreeTypeQuery((const FcChar8 *)regular_path, 0, blanks, &count);
    if (blanks) FcBlanksDestroy(blanks);
    if (!probe) return NULL;

    FcChar8 *fam = NULL;
    if (FcPatternGetString(probe, FC_FAMILY, 0, &fam) != FcResultMatch || !fam) {
        FcPatternDestroy(probe);
        return NULL;
    }

    FcPattern *pat = FcPatternCreate();
    FcPatternAddString(pat, FC_FAMILY, fam);
    FcPatternAddInteger(pat, FC_WEIGHT, FC_WEIGHT_BOLD);
    // Stay in the same spacing class so we don't get a proportional bold
    // for a monospace regular.
    int spacing = xt_fc_get_int(probe, FC_SPACING);
    if (spacing >= FC_MONO) {
        FcPatternAddInteger(pat, FC_SPACING, spacing);
    }
    FcPatternDestroy(probe);

    FcConfigSubstitute(NULL, pat, FcMatchPattern);
    FcDefaultSubstitute(pat);

    FcResult res;
    FcPattern *match = FcFontMatch(NULL, pat, &res);
    FcPatternDestroy(pat);
    if (!match) return NULL;

    char *out = NULL;
    FcChar8 *outpath = NULL;
    int outweight = FC_WEIGHT_REGULAR;
    FcPatternGetInteger(match, FC_WEIGHT, 0, &outweight);
    // Only return if fontconfig actually found a bold-or-heavier face;
    // its fallback may hand back the regular file when no bold exists.
    if (outweight >= FC_WEIGHT_SEMIBOLD &&
        FcPatternGetString(match, FC_FILE, 0, &outpath) == FcResultMatch) {
        // Refuse to point bold at the same file as regular — that produces
        // identical glyphs and confuses the renderer's bold logic. Caller
        // should fall through to faux-bold synthesis instead.
        if (strcmp((const char *)outpath, regular_path) != 0) {
            out = strdup((const char *)outpath);
        }
    }
    FcPatternDestroy(match);
    return out;
}

// ---- Font open / rasterize ----

static FT_Face xt_ft_open_font(const char *path) {
    if (!xt_ft_init()) return NULL;
    FT_Face face = NULL;
    if (FT_New_Face(xt_ft_lib, path, 0, &face) != 0) return NULL;
    return face;
}

static void xt_ft_close_font(FT_Face face) {
    if (face) FT_Done_Face(face);
}

static int xt_ft_font_has(FT_Face face, FT_ULong codepoint) {
    if (!face) return 0;
    return FT_Get_Char_Index(face, codepoint) != 0 ? 1 : 0;
}

// xt_ft_set_size requests pxSize from face. For scalable outline fonts
// this uses FT_Set_Pixel_Sizes (any size). For bitmap-only fonts (color
// emoji like NotoColorEmoji using CBDT, or Apple sbix), there are only
// fixed strikes; FT_Set_Pixel_Sizes would fail because no exact match
// exists. Fall back to FT_Select_Size against the largest available
// strike — the caller's renderer scales the bitmap down to cell size
// at draw time. Returns 0 on success, non-zero on failure.
static int xt_ft_set_size(FT_Face face, float pxSize) {
    if (!face) return -1;
    if (FT_IS_SCALABLE(face)) {
        return FT_Set_Pixel_Sizes(face, 0, (FT_UInt)ceil(pxSize));
    }
    if (face->num_fixed_sizes <= 0) return -1;
    // Pick the strike closest-to-or-larger-than pxSize so downscaling
    // dominates over upscaling (which looks worse).
    int best = 0;
    int bestDiff = 1 << 30;
    for (int i = 0; i < face->num_fixed_sizes; i++) {
        int strikePx = face->available_sizes[i].height;
        int diff = strikePx - (int)pxSize;
        if (diff < 0) diff = -diff;
        if (diff < bestDiff) {
            bestDiff = diff;
            best = i;
        }
    }
    return FT_Select_Size(face, best);
}

// xt_ft_metrics returns ascent, descent, and combined line-height at
// pxSize (matching the Darwin contract: LineHeight = ascent + descent + leading).
// Sets all values to 0 on failure.
static void xt_ft_metrics(FT_Face face, float pxSize, float *ascent, float *descent, float *line_height) {
    *ascent = 0; *descent = 0; *line_height = 0;
    if (!face) return;
    if (xt_ft_set_size(face, pxSize) != 0) return;
    // FT metrics are in 26.6 fixed-point (1 unit = 1/64 pixel) for size-relative
    // values. ascender is positive (above baseline), descender is negative
    // (below baseline) — flip its sign to match the positive-Down Darwin convention.
    *ascent = (float)face->size->metrics.ascender / 64.0f;
    *descent = -(float)face->size->metrics.descender / 64.0f;
    *line_height = (float)face->size->metrics.height / 64.0f;
}

// xt_ft_rasterize draws a single glyph into an RGBA bitmap allocated by
// the caller. out_pixels has a stride of max_w*4 RGBA bytes. Caller passes
// max_w*max_h*4 bytes. On success returns 1 and fills out_advance,
// out_bearing_x/y, out_is_color.
//
// synth_bold non-zero applies faux-bold via FT_GlyphSlot_Embolden, which
// algorithmically thickens the outline before rasterization.
static int xt_ft_rasterize(
    FT_Face face,
    FT_ULong codepoint,
    float pxSize,
    uint8_t *out_pixels,
    int *out_width,
    int *out_height,
    int *out_bearing_x,
    int *out_bearing_y,
    float *out_advance,
    int *out_is_color,
    int synth_bold,
    int max_w,
    int max_h
) {
    *out_is_color = 0;
    if (!face) return 0;
    if (xt_ft_set_size(face, pxSize) != 0) return 0;

    // FT_LOAD_COLOR enables color bitmap fonts (color emoji) where present.
    // Falls through to a normal grayscale render for monochrome fonts.
    FT_Int32 load_flags = FT_LOAD_COLOR | FT_LOAD_DEFAULT;
    if (FT_Load_Char(face, codepoint, load_flags) != 0) return 0;

    FT_GlyphSlot slot = face->glyph;
    if (slot->format == FT_GLYPH_FORMAT_OUTLINE && synth_bold) {
        // Faux-bold: embolden the outline by ~pxSize/16 pixels (in 26.6 fixed),
        // matching the Darwin stroke width.
        FT_Pos strength = (FT_Pos)(pxSize * 64.0f / 16.0f);
        if (strength < 32) strength = 32; // at least 0.5 px
        FT_Outline_Embolden(&slot->outline, strength);
    }

    if (slot->format != FT_GLYPH_FORMAT_BITMAP) {
        if (FT_Render_Glyph(slot, FT_RENDER_MODE_NORMAL) != 0) return 0;
    }

    FT_Bitmap *bm = &slot->bitmap;
    int w = (int)bm->width;
    int h = (int)bm->rows;
    if (w <= 0 || h <= 0) {
        // Empty glyph (e.g. space). Still report advance so the caller can
        // skip the bitmap copy and just bump the cursor.
        *out_width = 0;
        *out_height = 0;
        *out_bearing_x = 0;
        *out_bearing_y = 0;
        *out_advance = (float)slot->advance.x / 64.0f;
        return 1;
    }
    if (w > max_w) w = max_w;
    if (h > max_h) h = max_h;

    int is_color = 0;
    // Copy bitmap into out_pixels as premultiplied RGBA. Three pixel modes
    // we may receive:
    //   FT_PIXEL_MODE_GRAY:  monochrome glyph as 8-bit alpha mask (most fonts)
    //   FT_PIXEL_MODE_BGRA:  color bitmap (color emoji from CBDT/sbix tables)
    //   FT_PIXEL_MODE_MONO:  1-bit bitmap (rare; old bitmap fonts)
    for (int y = 0; y < h; y++) {
        const unsigned char *src = bm->buffer + y * bm->pitch;
        uint8_t *dst = out_pixels + y * max_w * 4;
        for (int x = 0; x < w; x++) {
            uint8_t r = 255, g = 255, b = 255, a = 0;
            switch (bm->pixel_mode) {
            case FT_PIXEL_MODE_GRAY:
                a = src[x];
                break;
            case FT_PIXEL_MODE_BGRA:
                // FreeType stores BGRA premultiplied. We emit RGBA premultiplied.
                b = src[x*4 + 0];
                g = src[x*4 + 1];
                r = src[x*4 + 2];
                a = src[x*4 + 3];
                if (a > 0 && (r != g || g != b)) is_color = 1;
                break;
            case FT_PIXEL_MODE_MONO:
                a = (src[x >> 3] & (0x80 >> (x & 7))) ? 255 : 0;
                break;
            default:
                a = 0;
                break;
            }
            dst[x*4 + 0] = r;
            dst[x*4 + 1] = g;
            dst[x*4 + 2] = b;
            dst[x*4 + 3] = a;
        }
    }
    *out_is_color = is_color;
    *out_width = w;
    *out_height = h;
    // bitmap_left = pixels from origin to left edge of bitmap (positive right).
    // bitmap_top = pixels from baseline up to top of bitmap (positive up).
    *out_bearing_x = slot->bitmap_left;
    *out_bearing_y = slot->bitmap_top;
    *out_advance = (float)slot->advance.x / 64.0f;
    return 1;
}
*/
import "C"

import (
	"fmt"
	"runtime"
	"strings"
	"unsafe"
)

// supportedFontExt reports whether a font file extension is one we know
// FreeType can render cleanly into the glyph cache. Belt-and-suspenders
// alongside the FC_SCALABLE filter in xt_fc_enum — fontconfig occasionally
// flags marginal formats (Type1, dfont) as scalable that we'd rather
// hide from the picker.
func supportedFontExt(path string) bool {
	switch strings.ToLower(path[strings.LastIndex(path, "."):]) {
	case ".ttf", ".otf", ".ttc", ".otc", ".woff", ".woff2":
		return true
	}
	return false
}

func init() {
	Default = linuxSystem{}
	IsVariableFont = func(path string) bool {
		cpath := C.CString(path)
		defer C.free(unsafe.Pointer(cpath))
		return C.xt_ft_is_variable_path(cpath) != 0
	}
}

type linuxSystem struct{}

// fontconfig property name C strings — declared once so we don't reallocate
// per Enumerate iteration.
var (
	fcPropFile     = C.CString(C.FC_FILE)
	fcPropFamily   = C.CString(C.FC_FAMILY)
	fcPropFullname = C.CString(C.FC_FULLNAME)
	fcPropStyle    = C.CString(C.FC_STYLE)
)

func (linuxSystem) Enumerate() ([]FontInfo, error) {
	fs := C.xt_fc_enum()
	if fs == nil {
		return nil, fmt.Errorf("fontconfig enumeration returned nil")
	}
	defer C.xt_fcset_destroy(fs)
	n := int(C.xt_fcset_count(fs))
	out := make([]FontInfo, 0, n)
	for i := 0; i < n; i++ {
		pat := C.xt_fcset_at(fs, C.int(i))
		fi := FontInfo{}
		if cs := C.xt_fc_get_string(pat, fcPropFile); cs != nil {
			fi.Path = C.GoString(cs)
			C.free(unsafe.Pointer(cs))
		}
		if cs := C.xt_fc_get_string(pat, fcPropFamily); cs != nil {
			fi.Family = C.GoString(cs)
			C.free(unsafe.Pointer(cs))
		}
		// Prefer the full PostScript-style name; fall back to family if absent.
		if cs := C.xt_fc_get_string(pat, fcPropFullname); cs != nil {
			fi.Name = C.GoString(cs)
			C.free(unsafe.Pointer(cs))
		} else {
			fi.Name = fi.Family
		}
		if cs := C.xt_fc_get_string(pat, fcPropStyle); cs != nil {
			fi.Style = C.GoString(cs)
			C.free(unsafe.Pointer(cs))
		}
		fi.Monospace = C.xt_fc_is_monospace(pat) != 0
		if fi.Path == "" {
			continue
		}
		if !supportedFontExt(fi.Path) {
			continue
		}
		// Skip variable fonts: ImGui's bundled stbtt parser asserts on
		// the fvar/gvar tables and crashes the app. The glyph cache
		// could in principle render them via FreeType, but the static
		// atlas (used for tab labels / dialog text) can't, and we use
		// the same picker for both.
		if C.xt_fc_is_variable(pat) != 0 {
			continue
		}
		out = append(out, fi)
	}
	return out, nil
}

func (linuxSystem) FindForCodepoint(r rune, hint string) (string, error) {
	var chint *C.char
	if hint != "" {
		chint = C.CString(hint)
		defer C.free(unsafe.Pointer(chint))
	}
	cs := C.xt_fc_find_for_codepoint(chint, C.FcChar32(r))
	if cs == nil {
		return "", nil
	}
	defer C.free(unsafe.Pointer(cs))
	return C.GoString(cs), nil
}

func (linuxSystem) Open(path string) (Font, error) {
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	face := C.xt_ft_open_font(cpath)
	if face == nil {
		return nil, fmt.Errorf("FreeType could not open %s", path)
	}
	f := &linuxFont{face: face, path: path}
	runtime.SetFinalizer(f, (*linuxFont).Close)
	return f, nil
}

type linuxFont struct {
	face          C.FT_Face
	path          string // remembered so Bold() can search via fontconfig
	syntheticBold bool   // when true, Rasterize applies faux-bold via FT_Outline_Embolden
}

func (f *linuxFont) Has(r rune) bool {
	if f.face == nil {
		return false
	}
	return C.xt_ft_font_has(f.face, C.FT_ULong(r)) != 0
}

func (f *linuxFont) LineMetrics(pxSize float32) LineMetrics {
	if f.face == nil {
		return LineMetrics{}
	}
	var asc, desc, lh C.float
	C.xt_ft_metrics(f.face, C.float(pxSize), &asc, &desc, &lh)
	return LineMetrics{
		Ascent:     float32(asc),
		Descent:    float32(desc),
		LineHeight: float32(lh),
	}
}

func (f *linuxFont) Rasterize(r rune, pxSize float32) (*Glyph, error) {
	if f.face == nil {
		return nil, fmt.Errorf("font not open")
	}
	const maxW, maxH = 256, 256
	pixels := make([]byte, maxW*maxH*4)
	var w, h, bx, by, isColor C.int
	var adv C.float
	synth := C.int(0)
	if f.syntheticBold {
		synth = 1
	}
	ok := C.xt_ft_rasterize(f.face, C.FT_ULong(r), C.float(pxSize),
		(*C.uint8_t)(unsafe.Pointer(&pixels[0])),
		&w, &h, &bx, &by, &adv, &isColor, synth,
		C.int(maxW), C.int(maxH),
	)
	if ok == 0 {
		return nil, nil
	}
	width := int(w)
	height := int(h)
	out := &Glyph{
		Width:    width,
		Height:   height,
		BearingX: int(bx),
		BearingY: int(by),
		Advance:  float32(adv),
		IsColor:  isColor != 0,
		Pixels:   make([]byte, width*height*4),
	}
	if width > 0 && height > 0 {
		// Trim the over-allocated bitmap down to actual glyph size.
		rowSrc := maxW * 4
		rowDst := width * 4
		for row := 0; row < height; row++ {
			copy(out.Pixels[row*rowDst:(row+1)*rowDst], pixels[row*rowSrc:row*rowSrc+rowDst])
		}
	}
	return out, nil
}

func (f *linuxFont) Bold() Font {
	if f.face == nil || f.path == "" {
		return nil
	}
	cpath := C.CString(f.path)
	defer C.free(unsafe.Pointer(cpath))
	cbold := C.xt_fc_find_bold(cpath)
	if cbold != nil {
		boldPath := C.GoString(cbold)
		C.free(unsafe.Pointer(cbold))
		face := C.xt_ft_open_font(C.CString(boldPath)) // small leak: cstr; fine, init-time
		if face != nil {
			bold := &linuxFont{face: face, path: boldPath}
			runtime.SetFinalizer(bold, (*linuxFont).Close)
			return bold
		}
	}
	// No real bold variant — synthesize via FT_Outline_Embolden in Rasterize.
	// Open a second FT_Face on the same file so the synthetic-bold instance
	// can Close() independently from the regular instance.
	cpath2 := C.CString(f.path)
	defer C.free(unsafe.Pointer(cpath2))
	face := C.xt_ft_open_font(cpath2)
	if face == nil {
		return nil
	}
	synth := &linuxFont{face: face, path: f.path, syntheticBold: true}
	runtime.SetFinalizer(synth, (*linuxFont).Close)
	return synth
}

func (f *linuxFont) Close() {
	if f.face != nil {
		C.xt_ft_close_font(f.face)
		f.face = nil
		runtime.SetFinalizer(f, nil)
	}
}

//go:build darwin

package fontsys

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework CoreText -framework CoreGraphics -framework CoreFoundation -framework Foundation

#include <CoreText/CoreText.h>
#include <CoreGraphics/CoreGraphics.h>
#include <CoreFoundation/CoreFoundation.h>
#include <stdlib.h>
#include <string.h>

// xt_enum_count returns how many installed font descriptors are available.
// Caller follows up with xt_enum_at to fetch each one.
static CFArrayRef xt_enum(void) {
    CTFontCollectionRef coll = CTFontCollectionCreateFromAvailableFonts(NULL);
    if (!coll) return NULL;
    CFArrayRef descs = CTFontCollectionCreateMatchingFontDescriptors(coll);
    CFRelease(coll);
    return descs;
}

// xt_desc_string copies a CFString attribute of a font descriptor into a
// C string the caller must free(). Returns NULL on miss.
static char *xt_desc_string(CTFontDescriptorRef desc, CFStringRef attr) {
    CFTypeRef val = CTFontDescriptorCopyAttribute(desc, attr);
    if (!val) return NULL;
    char *out = NULL;
    if (CFGetTypeID(val) == CFStringGetTypeID()) {
        CFStringRef s = (CFStringRef)val;
        CFIndex len = CFStringGetLength(s);
        CFIndex max = CFStringGetMaximumSizeForEncoding(len, kCFStringEncodingUTF8) + 1;
        out = (char *)malloc(max);
        if (!CFStringGetCString(s, out, max, kCFStringEncodingUTF8)) {
            free(out);
            out = NULL;
        }
    }
    CFRelease(val);
    return out;
}

// xt_desc_path returns the on-disk path for a font descriptor. Caller
// frees. NULL on miss.
static char *xt_desc_path(CTFontDescriptorRef desc) {
    CFURLRef url = (CFURLRef)CTFontDescriptorCopyAttribute(desc, kCTFontURLAttribute);
    if (!url) return NULL;
    char buf[2048];
    char *out = NULL;
    if (CFURLGetFileSystemRepresentation(url, true, (UInt8 *)buf, sizeof(buf))) {
        out = strdup(buf);
    }
    CFRelease(url);
    return out;
}

// xt_desc_monospace reports whether a descriptor's traits include the
// monospace bit (kCTFontMonoSpaceTrait).
static int xt_desc_monospace(CTFontDescriptorRef desc) {
    CFDictionaryRef traits = (CFDictionaryRef)CTFontDescriptorCopyAttribute(desc, kCTFontTraitsAttribute);
    if (!traits) return 0;
    int mono = 0;
    CFNumberRef sym = (CFNumberRef)CFDictionaryGetValue(traits, kCTFontSymbolicTrait);
    if (sym) {
        uint32_t bits = 0;
        CFNumberGetValue(sym, kCFNumberSInt32Type, &bits);
        if (bits & kCTFontMonoSpaceTrait) mono = 1;
    }
    CFRelease(traits);
    return mono;
}

static CTFontRef xt_open_font(const char *path);

// xt_find_for_codepoint asks CoreText which installed font is the best
// fallback for a given codepoint, biased by the selected primary font.
// hint may be a font path or a CoreText font name. Returns a malloc'd
// path the caller must free, or NULL if no font is found.
static char *xt_find_for_codepoint(const char *hint, uint32_t codepoint) {
    // Build a base font (hinted or system default) so CoreText has a
    // style anchor for cascade selection.
    CTFontRef base = NULL;
    if (hint && *hint) {
        if (strchr(hint, '/')) {
            base = xt_open_font(hint);
        }
        if (!base) {
            CFStringRef name = CFStringCreateWithCString(NULL, hint, kCFStringEncodingUTF8);
            if (name) {
                base = CTFontCreateWithName(name, 14.0, NULL);
                CFRelease(name);
            }
        }
    }
    if (!base) {
        base = CTFontCreateUIFontForLanguage(kCTFontUIFontUserFixedPitch, 14.0, NULL);
    }
    if (!base) return NULL;

    // CTFontCreateForString picks the right font (base if it has the
    // glyph, otherwise a cascade fallback).
    UniChar utf16[2];
    CFIndex utf16len;
    if (codepoint <= 0xFFFF) {
        utf16[0] = (UniChar)codepoint;
        utf16len = 1;
    } else {
        // Encode supplementary plane codepoint as a UTF-16 surrogate pair.
        uint32_t v = codepoint - 0x10000;
        utf16[0] = (UniChar)(0xD800 + (v >> 10));
        utf16[1] = (UniChar)(0xDC00 + (v & 0x3FF));
        utf16len = 2;
    }
    CFStringRef str = CFStringCreateWithCharacters(NULL, utf16, utf16len);
    if (!str) {
        CFRelease(base);
        return NULL;
    }

    CTFontRef chosen = CTFontCreateForString(base, str, CFRangeMake(0, utf16len));
    CFRelease(str);
    CFRelease(base);
    if (!chosen) return NULL;

    // Verify the chosen font actually has the codepoint (CTFontCreateForString
    // can return base unchanged if it gives up).
    CGGlyph glyph = 0;
    UniChar test = utf16[0];
    if (utf16len == 2) {
        // For supplementary plane, we'd need CTFontGetGlyphsForCharacters
        // with the surrogate pair — accept the cascade choice as-is.
        glyph = 1;
    } else {
        CTFontGetGlyphsForCharacters(chosen, &test, &glyph, 1);
    }
    char *out = NULL;
    if (glyph != 0) {
        CTFontDescriptorRef desc = CTFontCopyFontDescriptor(chosen);
        if (desc) {
            out = xt_desc_path(desc);
            CFRelease(desc);
        }
    }
    CFRelease(chosen);
    return out;
}

// xt_open_font loads a CTFontRef from a path. The returned ref is
// retained; xt_close_font releases it. Size is set later per-rasterize.
static CTFontRef xt_open_font(const char *path) {
    CFStringRef cfpath = CFStringCreateWithCString(NULL, path, kCFStringEncodingUTF8);
    if (!cfpath) return NULL;
    CFURLRef url = CFURLCreateWithFileSystemPath(NULL, cfpath, kCFURLPOSIXPathStyle, false);
    CFRelease(cfpath);
    if (!url) return NULL;
    CFArrayRef descs = CTFontManagerCreateFontDescriptorsFromURL(url);
    CFRelease(url);
    if (!descs || CFArrayGetCount(descs) == 0) {
        if (descs) CFRelease(descs);
        return NULL;
    }
    CTFontDescriptorRef desc = (CTFontDescriptorRef)CFArrayGetValueAtIndex(descs, 0);
    CTFontRef font = CTFontCreateWithFontDescriptor(desc, 14.0, NULL);
    CFRelease(descs);
    return font;
}

static void xt_close_font(CTFontRef font) {
    if (font) CFRelease(font);
}

// xt_bold_variant returns a bold-weight copy of regular. CoreText looks
// inside font collections (.ttc) to find a real bold face if one exists,
// and falls back to synthesizing faux-bold otherwise. Returns NULL if
// CoreText refuses entirely (rare).
static CTFontRef xt_bold_variant(CTFontRef regular) {
    if (!regular) return NULL;
    return CTFontCreateCopyWithSymbolicTraits(regular, 0, NULL,
        kCTFontBoldTrait, kCTFontBoldTrait);
}

// xt_font_has reports whether a font has a glyph for the codepoint.
// Handles both BMP (single UTF-16 unit) and supplementary plane
// (UTF-16 surrogate pair) so emoji and other SMP codepoints are
// correctly detected.
static int xt_font_has(CTFontRef font, uint32_t codepoint) {
    UniChar utf16[2];
    CGGlyph glyphs[2] = {0, 0};
    CFIndex len;
    if (codepoint <= 0xFFFF) {
        utf16[0] = (UniChar)codepoint;
        len = 1;
    } else {
        uint32_t v = codepoint - 0x10000;
        utf16[0] = (UniChar)(0xD800 + (v >> 10));
        utf16[1] = (UniChar)(0xDC00 + (v & 0x3FF));
        len = 2;
    }
    CTFontGetGlyphsForCharacters(font, utf16, glyphs, len);
    // For surrogate pairs the font returns one combined glyph in
    // glyphs[0] (and 0 in glyphs[1]); for BMP just glyphs[0].
    return glyphs[0] != 0 ? 1 : 0;
}

// xt_metrics returns ascent, descent, leading at pxSize.
static void xt_metrics(CTFontRef font, float pxSize, float *ascent, float *descent, float *leading) {
    CTFontRef sized = CTFontCreateCopyWithAttributes(font, pxSize, NULL, NULL);
    *ascent = (float)CTFontGetAscent(sized);
    *descent = (float)CTFontGetDescent(sized);
    *leading = (float)CTFontGetLeading(sized);
    CFRelease(sized);
}

// xt_rasterize draws a single glyph into an RGBA bitmap allocated by
// the caller. out_pixels has a stride of max_w*4 RGBA bytes. Caller
// must pass max_w*max_h*4 bytes (zeroing not required). On success
// returns 1 and fills out_advance / bearing / is_color.
//
// The bitmap origin (0,0) corresponds to the top-left of the bitmap.
// The caller positions the bitmap at (cursor_x + bearing_x,
// cursor_y - bearing_y) where cursor_y is the text baseline.
//
// synth_bold non-zero applies faux-bold: the glyph is drawn with both
// fill and stroke at the same color, producing a bolder appearance for
// fonts that ship only one weight (Monaco, etc).
static int xt_rasterize(
    CTFontRef font,
    uint32_t codepoint,
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
    CTFontRef sized = CTFontCreateCopyWithAttributes(font, pxSize, NULL, NULL);
    if (!sized) return 0;

    UniChar utf16[2];
    CFIndex utf16len;
    if (codepoint <= 0xFFFF) {
        utf16[0] = (UniChar)codepoint;
        utf16len = 1;
    } else {
        uint32_t v = codepoint - 0x10000;
        utf16[0] = (UniChar)(0xD800 + (v >> 10));
        utf16[1] = (UniChar)(0xDC00 + (v & 0x3FF));
        utf16len = 2;
    }

    CGGlyph glyphs[2] = {0, 0};
    CTFontGetGlyphsForCharacters(sized, utf16, glyphs, utf16len);
    if (glyphs[0] == 0) {
        CFRelease(sized);
        return 0;
    }

    // Measure glyph bounding box (in text-space pixels at pxSize).
    CGRect bbox = CTFontGetBoundingRectsForGlyphs(sized, kCTFontOrientationHorizontal, &glyphs[0], NULL, 1);
    CGSize advance;
    CTFontGetAdvancesForGlyphs(sized, kCTFontOrientationHorizontal, &glyphs[0], &advance, 1);
    int font_is_color = (CTFontGetSymbolicTraits(sized) & kCTFontColorGlyphsTrait) != 0;

    // Synthetic bold expands the glyph outline by stroke_width pixels
    // on each side, so reserve extra padding in the bitmap to avoid
    // clipping the stroke at the edges.
    float stroke_width = synth_bold ? pxSize / 16.0f : 0.0f;
    if (stroke_width < 0.5f && synth_bold) stroke_width = 0.5f;
    int w = (int)ceil(bbox.size.width) + 2 + (int)ceil(stroke_width * 2);
    int h = (int)ceil(bbox.size.height) + 2 + (int)ceil(stroke_width * 2);
    if (w <= 0 || h <= 0) {
        CFRelease(sized);
        return 0;
    }
    if (w > max_w) w = max_w;
    if (h > max_h) h = max_h;

    // Render into an RGBA8 scratch buffer. Pure grayscale contexts with
    // kCGImageAlphaNone are unreliable for CoreText glyph rendering on
    // macOS — many color/style code paths refuse to draw to them. RGBA
    // works everywhere, and we extract the alpha channel as our grayscale
    // glyph mask afterward.
    int rgba_size = w * h * 4;
    uint8_t *rgba = (uint8_t *)calloc(1, rgba_size);
    if (!rgba) {
        CFRelease(sized);
        return 0;
    }
    CGColorSpaceRef cs = CGColorSpaceCreateDeviceRGB();
    CGContextRef ctx = CGBitmapContextCreate(rgba, w, h, 8, w*4, cs,
        kCGImageAlphaPremultipliedLast | kCGBitmapByteOrder32Big);
    CGColorSpaceRelease(cs);
    if (!ctx) {
        free(rgba);
        CFRelease(sized);
        return 0;
    }

    // CGBitmapContext memory is row-major top-down, but CoreGraphics
    // drawing uses bottom-up coordinates: drawing at CG y=Y renders to
    // memory row (h-1-Y). We render in the natural orientation (no CTM
    // flip — flipping the CTM also flips glyph orientation, drawing
    // them upside-down). To put the glyph's bbox top at memory row 0,
    // place the baseline at CG y = (h - 1) - ascent_above_baseline.
    CGContextSetRGBFillColor(ctx, 1.0, 1.0, 1.0, 1.0);
    CGContextSetShouldAntialias(ctx, true);
    // Match AppKit/CoreText terminal rendering: Monaco's shade-block
    // glyphs (░▒▓) are built from tiny outline geometry, and disabling
    // font smoothing preserves those outlines as hollow squares instead
    // of the soft stipple produced by iTerm2/Terminal.app.
    CGContextSetAllowsFontSmoothing(ctx, true);
    CGContextSetShouldSmoothFonts(ctx, true);
    // Subpixel positioning/quantization matches AppKit's default text
    // placement and avoids hinted glyph variants drifting at fractional
    // positions.
    CGContextSetAllowsFontSubpixelPositioning(ctx, true);
    CGContextSetShouldSubpixelPositionFonts(ctx, true);
    CGContextSetAllowsFontSubpixelQuantization(ctx, true);
    CGContextSetShouldSubpixelQuantizeFonts(ctx, true);
    if (synth_bold) {
        CGContextSetRGBStrokeColor(ctx, 1.0, 1.0, 1.0, 1.0);
        CGContextSetLineWidth(ctx, stroke_width);
        CGContextSetTextDrawingMode(ctx, kCGTextFillStroke);
    }

    float ascent_above_baseline = bbox.origin.y + bbox.size.height;
    float baseline_y = (float)(h - 1) - ascent_above_baseline - 1.0f;
    CGPoint pos = CGPointMake(-bbox.origin.x + 1 + stroke_width, baseline_y);
    CTFontDrawGlyphs(sized, &glyphs[0], &pos, 1, ctx);

    CGContextRelease(ctx);

    // Copy the full RGBA buffer to caller's storage. CGBitmapContext
    // memory is row-major top-down; out_pixels uses a row stride of
    // max_w*4 so callers can sub-rect the glyph without copying again.
    // Color glyphs keep their natural RGB. Monochrome glyphs are stored
    // as a white alpha mask so foreground tinting remains correct.
    // With font smoothing enabled, CoreGraphics can encode coverage in
    // RGB while alpha stays too opaque for a mask, so derive monochrome
    // alpha from the rendered white RGB intensity instead of from A.
    int is_color = 0;
    for (int y = 0; y < h; y++) {
        int src = y * w * 4;
        int dst = y * max_w * 4;
        for (int x = 0; x < w; x++) {
            uint8_t r = rgba[src + x*4 + 0];
            uint8_t g = rgba[src + x*4 + 1];
            uint8_t b = rgba[src + x*4 + 2];
            uint8_t a = rgba[src + x*4 + 3];
            if (font_is_color) {
                out_pixels[dst + x*4 + 0] = r;
                out_pixels[dst + x*4 + 1] = g;
                out_pixels[dst + x*4 + 2] = b;
                if (a > 0 && (r != g || g != b)) is_color = 1;
            } else {
                a = (uint8_t)(((int)r * 77 + (int)g * 150 + (int)b * 29 + 128) >> 8);
                out_pixels[dst + x*4 + 0] = 255;
                out_pixels[dst + x*4 + 1] = 255;
                out_pixels[dst + x*4 + 2] = 255;
            }
            out_pixels[dst + x*4 + 3] = a;
        }
    }
    free(rgba);
    *out_is_color = is_color;

    *out_width = w;
    *out_height = h;
    *out_bearing_x = (int)floor(bbox.origin.x) - 1 - (int)stroke_width;
    *out_bearing_y = (int)ceil(ascent_above_baseline) + 1 + (int)stroke_width;
    *out_advance = (float)advance.width;

    CFRelease(sized);
    return 1;
}

// Helpers to walk the descriptors array since cgo can't index CFArray.
static CFIndex xt_array_count(CFArrayRef arr) { return arr ? CFArrayGetCount(arr) : 0; }
static CTFontDescriptorRef xt_array_at(CFArrayRef arr, CFIndex i) {
    return (CTFontDescriptorRef)CFArrayGetValueAtIndex(arr, i);
}
static void xt_array_release(CFArrayRef arr) { if (arr) CFRelease(arr); }

// Constants accessed via CGo because the kCT* symbols are CFStringRef.
static CFStringRef xt_attr_name(void) { return kCTFontNameAttribute; }
static CFStringRef xt_attr_family(void) { return kCTFontFamilyNameAttribute; }
static CFStringRef xt_attr_style(void) { return kCTFontStyleNameAttribute; }
*/
import "C"

import (
	"fmt"
	"runtime"
	"unsafe"
)

func init() {
	Default = darwinSystem{}
}

type darwinSystem struct{}

func (darwinSystem) Enumerate() ([]FontInfo, error) {
	arr := C.xt_enum()
	if arr == 0 {
		return nil, fmt.Errorf("CTFontCollection enumeration returned nil")
	}
	defer C.xt_array_release(arr)
	n := int(C.xt_array_count(arr))
	out := make([]FontInfo, 0, n)
	for i := 0; i < n; i++ {
		desc := C.xt_array_at(arr, C.CFIndex(i))
		fi := FontInfo{}
		if cs := C.xt_desc_string(desc, C.xt_attr_name()); cs != nil {
			fi.Name = C.GoString(cs)
			C.free(unsafe.Pointer(cs))
		}
		if cs := C.xt_desc_string(desc, C.xt_attr_family()); cs != nil {
			fi.Family = C.GoString(cs)
			C.free(unsafe.Pointer(cs))
		}
		if cs := C.xt_desc_string(desc, C.xt_attr_style()); cs != nil {
			fi.Style = C.GoString(cs)
			C.free(unsafe.Pointer(cs))
		}
		if cs := C.xt_desc_path(desc); cs != nil {
			fi.Path = C.GoString(cs)
			C.free(unsafe.Pointer(cs))
		}
		fi.Monospace = C.xt_desc_monospace(desc) != 0
		if fi.Path == "" {
			continue
		}
		out = append(out, fi)
	}
	return out, nil
}

func (darwinSystem) FindForCodepoint(r rune, hint string) (string, error) {
	var chint *C.char
	if hint != "" {
		chint = C.CString(hint)
		defer C.free(unsafe.Pointer(chint))
	}
	cs := C.xt_find_for_codepoint(chint, C.uint32_t(r))
	if cs == nil {
		return "", nil
	}
	defer C.free(unsafe.Pointer(cs))
	return C.GoString(cs), nil
}

func (darwinSystem) Open(path string) (Font, error) {
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	ref := C.xt_open_font(cpath)
	if ref == 0 {
		return nil, fmt.Errorf("CoreText could not open %s", path)
	}
	f := &darwinFont{ref: ref}
	runtime.SetFinalizer(f, (*darwinFont).Close)
	return f, nil
}

type darwinFont struct {
	ref           C.CTFontRef
	syntheticBold bool // when true, Rasterize applies faux-bold via fill+stroke
}

func (f *darwinFont) Has(r rune) bool {
	if f.ref == 0 {
		return false
	}
	return C.xt_font_has(f.ref, C.uint32_t(r)) != 0
}

func (f *darwinFont) LineMetrics(pxSize float32) LineMetrics {
	if f.ref == 0 {
		return LineMetrics{}
	}
	var asc, desc, lead C.float
	C.xt_metrics(f.ref, C.float(pxSize), &asc, &desc, &lead)
	return LineMetrics{
		Ascent:     float32(asc),
		Descent:    float32(desc),
		LineHeight: float32(asc + desc + lead),
	}
}

func (f *darwinFont) Rasterize(r rune, pxSize float32) (*Glyph, error) {
	if f.ref == 0 {
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
	ok := C.xt_rasterize(f.ref, C.uint32_t(r), C.float(pxSize),
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
	// Trim the over-allocated bitmap down to actual glyph size.
	rowSrc := maxW * 4
	rowDst := width * 4
	for row := 0; row < height; row++ {
		copy(out.Pixels[row*rowDst:(row+1)*rowDst], pixels[row*rowSrc:row*rowSrc+rowDst])
	}
	return out, nil
}

func (f *darwinFont) Bold() Font {
	if f.ref == 0 {
		return nil
	}
	boldRef := C.xt_bold_variant(f.ref)
	if boldRef != 0 {
		bold := &darwinFont{ref: boldRef}
		runtime.SetFinalizer(bold, (*darwinFont).Close)
		return bold
	}
	// No real bold weight available (e.g. Monaco) — return a font that
	// synthesizes faux-bold by stroking the glyph outline. We share the
	// underlying CTFontRef and retain it so both this and the original
	// can Close() independently.
	C.CFRetain(C.CFTypeRef(f.ref))
	synth := &darwinFont{ref: f.ref, syntheticBold: true}
	runtime.SetFinalizer(synth, (*darwinFont).Close)
	return synth
}

func (f *darwinFont) Close() {
	if f.ref != 0 {
		C.xt_close_font(f.ref)
		f.ref = 0
		runtime.SetFinalizer(f, nil)
	}
}

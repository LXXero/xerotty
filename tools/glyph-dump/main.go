package main

import (
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"sort"

	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/dpi"
	"github.com/LXXero/xerotty/internal/fontsys"
)

func main() {
	var outDir string
	var scale float64
	var pxSize float64
	var fontPath string
	var text string
	flag.StringVar(&outDir, "out", filepath.Join(os.TempDir(), "xerotty-glyph-dump"), "output directory")
	flag.Float64Var(&scale, "scale", 2.0, "framebuffer scale")
	flag.Float64Var(&pxSize, "px", 0, "logical font pixel size; default comes from config")
	flag.StringVar(&fontPath, "font", "", "font path; default comes from config")
	flag.StringVar(&text, "text", "░▒▓█M", "text whose runes should be dumped")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		fatalf("load config: %v", err)
	}
	if fontPath == "" {
		fontPath = cfg.Font.Path
	}
	if fontPath == "" {
		fatalf("config has no font.path; pass -font")
	}
	if pxSize <= 0 {
		pxSize = float64(dpi.PointsToPixels(cfg.Font.Size))
	}
	if scale <= 0 {
		scale = 1
	}
	if err := os.MkdirAll(outDir, 0755); err != nil {
		fatalf("create output dir: %v", err)
	}
	if fontsys.Default == nil {
		fatalf("no fontsys implementation for this platform")
	}
	font, err := fontsys.Default.Open(fontPath)
	if err != nil {
		fatalf("open font: %v", err)
	}
	defer font.Close()
	fallbacks := map[string]fontsys.Font{}
	defer func() {
		for _, f := range fallbacks {
			f.Close()
		}
	}()

	runes := []rune(text)
	rasterPx := float32(pxSize * scale)
	metrics := font.LineMetrics(rasterPx)
	fmt.Printf("font: %s\n", fontPath)
	fmt.Printf("logical px: %.2f  fb scale: %.2f  raster px: %.2f\n", pxSize, scale, rasterPx)
	fmt.Printf("primary metrics physical: ascent=%.2f descent=%.2f line=%.2f\n", metrics.Ascent, metrics.Descent, metrics.LineHeight)
	fmt.Printf("out: %s\n", outDir)

	for _, r := range runes {
		source := "primary"
		rasterFont := font
		if !font.Has(r) {
			source = "missing"
			path, _ := fontsys.Default.FindForCodepoint(r, fontPath)
			if path != "" {
				source = "fallback:" + path
				rasterFont = fallbacks[path]
				if rasterFont == nil {
					f, err := fontsys.Default.Open(path)
					if err != nil {
						fmt.Printf("U+%04X %q: fallback open failed: %s: %v\n", r, string(r), path, err)
						continue
					}
					fallbacks[path] = f
					rasterFont = f
				}
			}
		}
		g, err := rasterFont.Rasterize(r, rasterPx)
		if err != nil {
			fatalf("rasterize U+%04X: %v", r, err)
		}
		if g == nil {
			fmt.Printf("U+%04X %q: missing source=%s\n", r, string(r), source)
			continue
		}
		prefix := filepath.Join(outDir, fmt.Sprintf("U+%04X", r))
		if err := writeRGBA(prefix+"-rgba.png", g.Width, g.Height, g.Pixels); err != nil {
			fatalf("write rgba: %v", err)
		}
		if err := writeMask(prefix+"-mask.png", g.Width, g.Height, g.Pixels); err != nil {
			fatalf("write mask: %v", err)
		}
		if err := writeComposite(prefix+"-on-black.png", g.Width, g.Height, g.Pixels, color.RGBA{R: 255, G: 92, B: 57, A: 255}); err != nil {
			fatalf("write composite: %v", err)
		}
		if err := os.WriteFile(prefix+"-alpha.txt", []byte(alphaReport(g.Width, g.Height, g.Pixels)), 0644); err != nil {
			fatalf("write alpha report: %v", err)
		}
		fmt.Printf("U+%04X %q: %dx%d bearing=(%d,%d) advance=%.2f color=%v source=%s\n",
			r, string(r), g.Width, g.Height, g.BearingX, g.BearingY, g.Advance, g.IsColor, source)
	}
}

func writeRGBA(path string, w, h int, pixels []byte) error {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	copy(img.Pix, pixels)
	return writePNG(path, img)
}

func writeMask(path string, w, h int, pixels []byte) error {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			a := pixels[(y*w+x)*4+3]
			img.SetRGBA(x, y, color.RGBA{R: a, G: a, B: a, A: 255})
		}
	}
	return writePNG(path, img)
}

func writeComposite(path string, w, h int, pixels []byte, fg color.RGBA) error {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			a := uint32(pixels[(y*w+x)*4+3])
			img.SetRGBA(x, y, color.RGBA{
				R: uint8((uint32(fg.R) * a) / 255),
				G: uint8((uint32(fg.G) * a) / 255),
				B: uint8((uint32(fg.B) * a) / 255),
				A: 255,
			})
		}
	}
	return writePNG(path, img)
}

func writePNG(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

func alphaReport(w, h int, pixels []byte) string {
	hist := map[uint8]int{}
	for i := 3; i < len(pixels); i += 4 {
		hist[pixels[i]]++
	}
	keys := make([]int, 0, len(hist))
	for a := range hist {
		keys = append(keys, int(a))
	}
	sort.Ints(keys)

	out := fmt.Sprintf("size=%dx%d pixels=%d unique_alpha=%d\n", w, h, w*h, len(keys))
	out += "histogram:\n"
	for _, k := range keys {
		out += fmt.Sprintf("  %3d: %d\n", k, hist[uint8(k)])
	}
	out += "\npreview:\n"
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			a := pixels[(y*w+x)*4+3]
			switch {
			case a == 0:
				out += " "
			case a < 48:
				out += "."
			case a < 96:
				out += ":"
			case a < 144:
				out += "*"
			case a < 192:
				out += "o"
			case a < 240:
				out += "O"
			default:
				out += "#"
			}
		}
		out += "\n"
	}
	return out
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "glyph-dump: "+format+"\n", args...)
	os.Exit(1)
}

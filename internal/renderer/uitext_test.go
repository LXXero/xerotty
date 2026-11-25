package renderer

import (
	"os"
	"testing"

	"github.com/AllenDang/cimgui-go/imgui"
)

// TestNoteUITextMergesFallback: a tab-label character the atlas font
// lacks (a Codex braille spinner frame) is looked up once, and after
// ApplyUIFallbacks the font has it.
func TestNoteUITextMergesFallback(t *testing.T) {
	const primary = "/usr/share/fonts/noto/NotoSansMono-Regular.ttf"
	const braille = "/usr/share/fonts/TTF/DejaVuSans.ttf"
	for _, p := range []string{primary, braille} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("needs %s", p)
		}
	}
	ctx := imgui.CreateContext()
	defer imgui.DestroyContextV(ctx)
	io := imgui.CurrentIO()
	io.SetBackendFlags(io.BackendFlags() | imgui.BackendFlagsRendererHasTextures)

	font := io.Fonts().AddFontFromFileTTFV(primary, 16, imgui.NewFontConfig(), terminalGlyphRanges())
	uiTextReset(font, primary, 16)
	t.Cleanup(func() { uiText.font, uiText.merged = nil, nil })

	if font.IsGlyphInFont(imgui.Wchar('⠸')) {
		t.Skip("primary already has braille")
	}
	var looked []rune
	old := uiFallbackFind
	uiFallbackFind = func(r rune, _ string) string { looked = append(looked, r); return braille }
	t.Cleanup(func() { uiFallbackFind = old })

	if !NoteUIText("⠸ Read brief") {
		t.Fatal("NoteUIText queued nothing for a missing braille glyph")
	}
	if NoteUIText("⠸ Read brief") {
		t.Fatal("second call queued again")
	}
	if len(looked) != 1 || looked[0] != '⠸' {
		t.Fatalf("looked up %q, want only ⠸", string(looked))
	}
	if !ApplyUIFallbacks() {
		t.Fatal("ApplyUIFallbacks merged nothing")
	}
	if !font.IsGlyphInFont(imgui.Wchar('⠸')) {
		t.Fatal("font still lacks ⠸ after the merge")
	}
	// ⠼ is in the merged font now: nothing to queue.
	if NoteUIText("⠼") {
		t.Fatal("queued a font for a glyph the merged font already covers")
	}
}

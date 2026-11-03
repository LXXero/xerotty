package input

import "testing"

func TestPickMIMEPrefersWantedOrder(t *testing.T) {
	// A Qt/KDE owner (flameshot) offers many image types with PNG far
	// down the list; our order, not the owner's, must win.
	offered := []string{"application/x-qt-image", "image/bmp", "image/jpeg", "image/png", "image/webp"}
	if got := pickMIME(offered, imagePasteMIMEs); got != "image/png" {
		t.Fatalf("pickMIME = %q, want image/png", got)
	}
	// Text aliases: a wl-copy owner offers only legacy atoms first.
	offered = []string{"TEXT", "STRING", "text/plain;charset=utf-8"}
	if got := pickMIME(offered, textMIMEs); got != "text/plain;charset=utf-8" {
		t.Fatalf("pickMIME = %q, want text/plain;charset=utf-8", got)
	}
	if got := pickMIME([]string{"image/png"}, textMIMEs); got != "" {
		t.Fatalf("pickMIME on image-only offer = %q, want empty", got)
	}
	if got := pickMIME(nil, textMIMEs); got != "" {
		t.Fatalf("pickMIME on empty clipboard = %q, want empty", got)
	}
}

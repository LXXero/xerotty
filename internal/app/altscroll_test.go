package app

import "testing"

// altScrollSeq must emit the right cursor-key sequence per direction and
// DECCKM state — wrong bytes would scroll the wrong way or not at all in
// mutt/less/vim.
func TestAltScrollSeq(t *testing.T) {
	cases := []struct {
		up, app bool
		want    string
	}{
		{true, false, "\x1b[A"},  // up, normal
		{false, false, "\x1b[B"}, // down, normal
		{true, true, "\x1bOA"},   // up, application cursor keys
		{false, true, "\x1bOB"},  // down, application cursor keys
	}
	for _, c := range cases {
		if got := string(altScrollSeq(c.up, c.app)); got != c.want {
			t.Errorf("altScrollSeq(up=%v, app=%v) = %q, want %q", c.up, c.app, got, c.want)
		}
	}
}

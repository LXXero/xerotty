package terminal

import (
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	vt "github.com/charmbracelet/x/vt"
)

// TestDiskScrollbackKeepsLinks: an OSC 8 target survives the disk store
// and the cells after a linked cell stay aligned.
func TestDiskScrollbackKeepsLinks(t *testing.T) {
	const url = "https://example.com/a"
	line := uv.Line{
		{Content: "a", Width: 1},
		{Content: "b", Width: 1, Link: uv.Link{URL: url}},
		{Content: "語", Width: 2, Link: uv.Link{URL: url}},
		{Content: "", Width: 0},
		{Content: "c", Width: 1},
	}
	got := decodeLine(encodeLine(line))
	if len(got) != len(line) {
		t.Fatalf("decoded %d cells", len(got))
	}
	if got[0].Link.URL != "" || got[1].Link.URL != url || got[2].Link.URL != url || got[4].Link.URL != "" {
		t.Fatalf("links = %+v", got)
	}
	if got[2].Width != 2 || got[4].Content != "c" {
		t.Fatalf("cells after a link misaligned: %+v", got)
	}
}

// TestDiskScrollbackReadsRecordsWithoutLinks: a hot upgrade hands the
// old daemon's file to the new one, so a record written before links
// were stored must decode. Such a record is byte-for-byte what
// encodeLine still writes for a line with no links.
func TestDiskScrollbackReadsRecordsWithoutLinks(t *testing.T) {
	old := []byte{
		2,               // cell count
		1, 'a', 1, 0, 0, // "a", width 1, no attrs, default colors
		1, 'b', 1, 0, 0,
	}
	if enc := encodeLine(uv.Line{{Content: "a", Width: 1}, {Content: "b", Width: 1}}); string(enc) != string(old) {
		t.Fatalf("format without links changed: %v", enc)
	}
	got := decodeLine(old)
	if len(got) != 2 || got[1].Content != "b" || got[1].Width != 1 {
		t.Fatalf("decoded %+v", got)
	}
}

// TestDiskScrollbackMalformedLink: a truncated link record decodes to
// the cells before it, without panicking.
func TestDiskScrollbackMalformedLink(t *testing.T) {
	line := uv.Line{{Content: "a", Width: 1}, {Content: "b", Width: 1, Link: uv.Link{URL: "https://example.com/"}}}
	enc := encodeLine(line)
	if got := decodeLine(enc[:len(enc)-3]); len(got) != 1 {
		t.Fatalf("decoded %+v", got)
	}
}

// TestScrollbackRowOneDiskRead: reading a row must cost one disk decode,
// not one per column, and carry the link. Hit-testing does this on every
// frame the mouse rests on text.
func TestScrollbackRowOneDiskRead(t *testing.T) {
	const cols = 80
	const url = "https://example.com/a"
	disk, err := NewDiskScrollback()
	if err != nil {
		t.Fatalf("disk: %v", err)
	}
	line := make(uv.Line, cols)
	for c := range line {
		line[c] = uv.Cell{Content: "x", Width: 1, Link: uv.Link{URL: url}}
	}
	if err := disk.Append(line); err != nil {
		t.Fatalf("append: %v", err)
	}
	term := &Terminal{Emu: vt.NewSafeEmulator(cols, 24), disk: disk}

	before := disk.ReadCount()
	row := term.ScrollbackRow(0, cols)
	if reads := disk.ReadCount() - before; reads != 1 {
		t.Fatalf("%d disk reads for one row", reads)
	}
	if len(row) != cols || row[cols-1].Link.URL != url {
		t.Fatalf("row[%d] = %+v", cols-1, row[cols-1])
	}
}

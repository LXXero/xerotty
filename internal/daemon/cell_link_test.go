package daemon

import (
	"testing"

	"github.com/LXXero/xerotty/internal/protocol"
)

// TestCellDiffSeesLinkChange: text that keeps its look and gains a link
// is a change the client has to receive.
func TestCellDiffSeesLinkChange(t *testing.T) {
	prev := [][]protocol.Cell{{{Content: "x", Width: 1}}}
	cur := [][]protocol.Cell{{{Content: "x", Width: 1, Link: "https://example.com/"}}}
	diff := computeCellDiff(1, prev, cur)
	if diff == nil || len(diff.Cells) != 1 || diff.Cells[0].Cell.Link == "" {
		t.Fatalf("diff = %+v", diff)
	}
}

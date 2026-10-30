package cellconv

import (
	"strings"
	"testing"

	"github.com/LXXero/xerotty/internal/protocol"
	uv "github.com/charmbracelet/ultraviolet"
)

func TestLinkSurvivesWireRoundTrip(t *testing.T) {
	const url = "https://example.com/a"
	in := uv.Cell{Content: "x", Width: 1, Link: uv.Link{URL: url}}
	wire := FromUV(&in)
	if wire.Link != url {
		t.Fatalf("wire link = %q", wire.Link)
	}
	if out := ToUV(wire); out.Link.URL != url {
		t.Fatalf("decoded link = %q", out.Link.URL)
	}
}

// TestCapFrameLinks: a screen full of long links must stay well under
// the 16 MiB frame limit, keeping the cells' text.
func TestCapFrameLinks(t *testing.T) {
	url := strings.Repeat("a", 8192)
	rows := make([][]protocol.Cell, 100)
	for r := range rows {
		rows[r] = make([]protocol.Cell, 400)
		for c := range rows[r] {
			rows[r][c] = protocol.Cell{Content: "x", Width: 1, Link: url}
		}
	}
	CapFrameLinks(rows)

	total := 0
	for _, row := range rows {
		for _, c := range row {
			total += len(c.Link)
			if c.Content != "x" {
				t.Fatal("text changed")
			}
		}
	}
	if total == 0 || total > maxFrameLinkBytes {
		t.Fatalf("%d link bytes left", total)
	}
}

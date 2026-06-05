package handoff

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/LXXero/xerotty/internal/protocol"
)

func sampleState() *State {
	return &State{
		InstanceID:   "inst-abc",
		NextTabID:    7,
		NextWindowID: 3,
		Revision:     42,
		Windows: []protocol.WindowInfo{
			{ID: 1, TabIDs: []uint32{1, 4}, FocusedTabID: 4, Width: 800, Height: 600},
		},
		Tabs: []TabState{
			{
				ID: 1, Name: "build", Title: "make", CWD: "/home/x", Cols: 80, Rows: 24,
				PtmxFD: 7, ChildPID: 4242,
				Screen: [][]protocol.Cell{
					{{Content: "$", Width: 1}, {Content: " ", Width: 1}},
					{{Content: "宽", Width: 2}, {Content: " ", Width: 0}},
				},
				CursorRow: 0, CursorCol: 2, AppCursor: true,
				MemScrollback: [][]protocol.Cell{{{Content: "old", Width: 1}}},
				DiskFD:        9, DiskOffsets: []int64{0, 128, 256}, DiskSize: 384,
			},
			{ID: 4, Cols: 120, Rows: 40, PtmxFD: 11, ChildPID: 4243, DiskFD: -1},
		},
		WireListenFD: 3, MCPListenFD: 4,
		SocketPath: "/run/user/1000/xerottyd.sock",
		MCPSocket:  "/run/user/1000/xerottyd.mcp.sock",
	}
}

func TestRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "handoff.bin")
	in := sampleState()
	if err := in.WriteFile(path); err != nil {
		t.Fatalf("write: %v", err)
	}
	out, err := ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if out.Version != Version {
		t.Fatalf("version: %d", out.Version)
	}
	if out.InstanceID != in.InstanceID || out.Revision != in.Revision ||
		out.NextTabID != in.NextTabID || len(out.Tabs) != 2 || len(out.Windows) != 1 {
		t.Fatalf("top-level fields mangled: %+v", out)
	}
	tb := out.Tabs[0]
	if tb.PtmxFD != 7 || tb.ChildPID != 4242 || !tb.AppCursor || tb.Name != "build" {
		t.Fatalf("tab fields mangled: %+v", tb)
	}
	if len(tb.Screen) != 2 || tb.Screen[1][0].Content != "宽" || tb.Screen[1][0].Width != 2 {
		t.Fatalf("screen cells mangled: %+v", tb.Screen)
	}
	if len(tb.DiskOffsets) != 3 || tb.DiskOffsets[2] != 256 || tb.DiskSize != 384 {
		t.Fatalf("disk index mangled: %+v", tb)
	}
	if out.Tabs[1].DiskFD != -1 {
		t.Fatalf("no-disk tab should keep DiskFD -1: %+v", out.Tabs[1])
	}
}

// TestVersionGate is the safety property the pre-exec validation
// run leans on: an unknown version is a clean refusal.
func TestVersionGate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "handoff.bin")
	in := sampleState()
	if err := in.WriteFile(path); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Rewrite with a future version.
	in.Version = Version + 1
	b, err := in.MarshalMsg(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeRaw(path, b); err != nil {
		t.Fatal(err)
	}
	_, err = ReadFile(path)
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("future version must refuse cleanly, got %v", err)
	}
}

func TestReadMissingFile(t *testing.T) {
	if _, err := ReadFile(filepath.Join(t.TempDir(), "nope.bin")); err == nil {
		t.Fatal("missing file must error")
	}
}

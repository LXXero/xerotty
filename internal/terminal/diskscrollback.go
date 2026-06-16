// Disk-backed scrollback storage. Used when Scrollback.Mode ==
// "unlimited" so a terminal session can hold an effectively
// unbounded history without ballooning RAM. Modeled after VTE's
// VteFileStream (xfce4-terminal / gnome-terminal use this through
// the GTK terminal widget): old lines spill from the vt emulator's
// in-memory ring into a temp file; recent lines stay in RAM where
// reads are free; scrolling far back triggers a seek+read.
//
// File layout: append-only, one line per record. Record format:
//
//   varint  line_byte_size  (the rest of the record)
//   varint  cell_count
//   for each cell:
//     varint  content_byte_size
//     bytes   content (UTF-8, may be a grapheme cluster)
//     u8      width (1 or 2)
//     u8      attrs
//     u8      fg_kind | (bg_kind << 4)
//     bytes   fg payload (variable length based on kind)
//     bytes   bg payload
//
// Color kinds: 0 = default (nil), 1 = ansi.BasicColor (1 byte),
// 2 = ansi.ExtendedColor (1 byte), 3 = ansi.TrueColor (3 bytes),
// 4 = color.RGBA (4 bytes).
//
// Temp file is unlinked immediately after creation: the inode lives
// only via our open fd, so the OS reclaims its space the moment the
// process exits. No orphaned files in /tmp.

package terminal

import (
	"encoding/binary"
	"fmt"
	"image/color"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"github.com/charmbracelet/x/ansi"
	uv "github.com/charmbracelet/ultraviolet"
)

// DiskScrollback is the append-only on-disk store for old scrollback
// lines. Safe for concurrent Append from the PTY-reader goroutine
// and LineAt from the render goroutine.
type DiskScrollback struct {
	mu      sync.Mutex
	f       *os.File
	offsets []int64 // offsets[i] is the byte offset of line i in f
	size    int64   // current file size (next append position)
	closed  bool
	reads   atomic.Int64 // LineAt count — read budget guard (see ReadCount)
}

// ReadCount returns how many line reads (disk decodes) have happened.
// Used by tests to guard against per-cell read amplification in bulk
// snapshots — the bug that hung the daemon under the on-demand fetch.
func (d *DiskScrollback) ReadCount() int64 { return d.reads.Load() }

// NewDiskScrollback creates an empty disk-backed scrollback store
// in a temp file that's unlinked immediately, so the file's inode
// vanishes when the process exits (no orphans).
func NewDiskScrollback() (*DiskScrollback, error) {
	dir := os.TempDir()
	f, err := os.CreateTemp(dir, "xerotty-scrollback-*")
	if err != nil {
		return nil, fmt.Errorf("create temp scrollback file: %w", err)
	}
	// Unlink while we hold the fd: file is gone from the filesystem
	// but readable/writable through our handle until close.
	_ = os.Remove(f.Name())
	return &DiskScrollback{f: f}, nil
}

// Append writes one line to the end of the store. Thread-safe.
func (d *DiskScrollback) Append(line uv.Line) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return os.ErrClosed
	}

	buf := encodeLine(line)
	header := make([]byte, binary.MaxVarintLen64)
	n := binary.PutUvarint(header, uint64(len(buf)))

	off := d.size
	if _, err := d.f.WriteAt(header[:n], off); err != nil {
		return err
	}
	if _, err := d.f.WriteAt(buf, off+int64(n)); err != nil {
		return err
	}
	d.offsets = append(d.offsets, off)
	d.size = off + int64(n) + int64(len(buf))
	return nil
}

// LineAt reads back the line at the given 0-based index. Returns
// nil if idx is out of range or on read error (which is rare for a
// freshly-written temp file; we don't surface it to the renderer).
func (d *DiskScrollback) LineAt(idx int) uv.Line {
	d.mu.Lock()
	defer d.mu.Unlock()
	if idx < 0 || idx >= len(d.offsets) || d.closed {
		return nil
	}
	d.reads.Add(1)

	off := d.offsets[idx]
	// Length is varint at the start of the record. Read up to 10
	// bytes and decode; the actual line payload follows.
	var hdrBuf [binary.MaxVarintLen64]byte
	hdrN, err := d.f.ReadAt(hdrBuf[:], off)
	if err != nil && err != io.EOF {
		return nil
	}
	bodyLen, n := binary.Uvarint(hdrBuf[:hdrN])
	if n <= 0 {
		return nil
	}
	body := make([]byte, bodyLen)
	if _, err := d.f.ReadAt(body, off+int64(n)); err != nil {
		return nil
	}
	return decodeLine(body)
}

// Len reports how many lines are stored. Thread-safe.
func (d *DiskScrollback) Len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.offsets)
}

// Clear drops all stored lines. Truncates the backing file in place
// (cheaper than close + reopen — keeps the temp inode alive).
// Thread-safe.
func (d *DiskScrollback) Clear() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.f == nil {
		return nil
	}
	if err := d.f.Truncate(0); err != nil {
		return err
	}
	if _, err := d.f.Seek(0, 0); err != nil {
		return err
	}
	d.offsets = d.offsets[:0]
	// Reset d.size too — Append() uses it as the next write
	// offset. Forgetting this leaves d.size pointing past the
	// (now-empty) file end so the next Append writes at the
	// stale offset, creating a sparse file with garbage between
	// 0 and the old size + offsets[] pointing past EOF until the
	// file refills.
	d.size = 0
	return nil
}

// Close releases the file handle. Safe to call from any goroutine.
// The temp file's already unlinked, so close finishes the cleanup.
func (d *DiskScrollback) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	if d.f == nil {
		return nil
	}
	return d.f.Close()
}

// encodeLine serializes a line to the wire format described in the
// package docstring.
func encodeLine(line uv.Line) []byte {
	// Pre-allocate roughly: per cell ~10-14 bytes.
	out := make([]byte, 0, 16+len(line)*12)

	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(tmp[:], uint64(len(line)))
	out = append(out, tmp[:n]...)

	for i := range line {
		cell := &line[i]

		contentBytes := []byte(cell.Content)
		n = binary.PutUvarint(tmp[:], uint64(len(contentBytes)))
		out = append(out, tmp[:n]...)
		out = append(out, contentBytes...)

		w := uint8(cell.Width)
		if w == 0 {
			w = 1
		}
		out = append(out, w)
		out = append(out, cell.Style.Attrs)

		fgKind, fgPayload := encodeColor(cell.Style.Fg)
		bgKind, bgPayload := encodeColor(cell.Style.Bg)
		out = append(out, fgKind|(bgKind<<4))
		out = append(out, fgPayload...)
		out = append(out, bgPayload...)
	}
	return out
}

// decodeLine deserializes one line. Returns an empty line on a
// malformed record rather than nil so the renderer renders blanks
// (less jarring than crashing).
func decodeLine(buf []byte) uv.Line {
	pos := 0
	cellCount, n := binary.Uvarint(buf[pos:])
	if n <= 0 {
		return uv.Line{}
	}
	pos += n

	cells := make(uv.Line, 0, cellCount)
	for i := uint64(0); i < cellCount; i++ {
		if pos >= len(buf) {
			break
		}
		contentLen, n := binary.Uvarint(buf[pos:])
		if n <= 0 {
			break
		}
		pos += n
		if pos+int(contentLen) > len(buf) {
			break
		}
		content := string(buf[pos : pos+int(contentLen)])
		pos += int(contentLen)
		if pos+3 > len(buf) {
			break
		}
		width := buf[pos]
		pos++
		attrs := buf[pos]
		pos++
		kinds := buf[pos]
		pos++
		fgKind := kinds & 0xF
		bgKind := (kinds >> 4) & 0xF

		fg, consumed := decodeColor(fgKind, buf[pos:])
		pos += consumed
		bg, consumed := decodeColor(bgKind, buf[pos:])
		pos += consumed

		cells = append(cells, uv.Cell{
			Content: content,
			Style: uv.Style{
				Fg:    fg,
				Bg:    bg,
				Attrs: attrs,
			},
			Width: int(width),
		})
	}
	return cells
}

// encodeColor returns (kind, payload). Kind 0 = nil/default; the
// other kinds map to ansi.BasicColor / ExtendedColor / TrueColor and
// color.RGBA. We preserve the original ANSI type so subsequent theme
// changes still recolor disk-stored lines correctly.
func encodeColor(c color.Color) (uint8, []byte) {
	switch v := c.(type) {
	case nil:
		return 0, nil
	case ansi.BasicColor:
		return 1, []byte{byte(v)}
	case ansi.ExtendedColor:
		return 2, []byte{byte(v)}
	case ansi.TrueColor:
		return 3, []byte{
			byte((uint32(v) >> 16) & 0xff),
			byte((uint32(v) >> 8) & 0xff),
			byte(uint32(v) & 0xff),
		}
	}
	// Fallback: any color.Color that's not an ansi.* variant. Read
	// its RGBA() and store as 4 bytes (8-bit per channel).
	r, g, b, a := c.RGBA()
	return 4, []byte{byte(r >> 8), byte(g >> 8), byte(b >> 8), byte(a >> 8)}
}

func decodeColor(kind uint8, buf []byte) (color.Color, int) {
	switch kind {
	case 0:
		return nil, 0
	case 1:
		if len(buf) < 1 {
			return nil, 0
		}
		return ansi.BasicColor(buf[0]), 1
	case 2:
		if len(buf) < 1 {
			return nil, 0
		}
		return ansi.ExtendedColor(buf[0]), 1
	case 3:
		if len(buf) < 3 {
			return nil, 0
		}
		return ansi.TrueColor(uint32(buf[0])<<16 | uint32(buf[1])<<8 | uint32(buf[2])), 3
	case 4:
		if len(buf) < 4 {
			return nil, 0
		}
		return color.RGBA{R: buf[0], G: buf[1], B: buf[2], A: buf[3]}, 4
	}
	return nil, 0
}

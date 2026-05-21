package protocol

import (
	"bytes"
	"io"
	"testing"
)

// TestFrameRoundTrip writes a few frames into a buffer and reads
// them back out. Smoke test for the codec — guards against length-
// prefix endianness mistakes and short-write bugs.
func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer

	in := []struct {
		typ  MsgType
		body Msg
	}{
		{MsgHello, &Hello{Version: 1, ClientID: "test"}},
		{MsgAttach, &Attach{SessionName: "default", NewIfMissing: true}},
		{MsgInputBytes, &InputBytes{ID: 7, Bytes: []byte("ls\r")}},
		{MsgBell, &Bell{ID: 42}},
		{MsgError, &Error{Code: 1, Message: "bad request"}},
	}

	for _, msg := range in {
		if err := WriteFrame(&buf, msg.typ, msg.body); err != nil {
			t.Fatalf("WriteFrame %v: %v", msg.typ, err)
		}
	}

	r := NewFrameReader(&buf)
	for i, want := range in {
		gotType, body, err := r.ReadFrame()
		if err != nil {
			t.Fatalf("frame %d: ReadFrame: %v", i, err)
		}
		if gotType != want.typ {
			t.Errorf("frame %d: type = %v, want %v", i, gotType, want.typ)
		}
		// We can't easily round-trip via the Msg interface (it's
		// marshal-only on purpose), so just confirm the body
		// payload survived — typed unmarshal happens at the call
		// site for each message type.
		if len(body) == 0 {
			t.Errorf("frame %d: empty body", i)
		}
	}

	if _, _, err := r.ReadFrame(); err != io.EOF {
		t.Errorf("expected EOF after draining, got %v", err)
	}
}

// TestFrameTooLarge confirms the reader rejects garbage length
// prefixes instead of attempting a huge allocation.
func TestFrameTooLarge(t *testing.T) {
	var buf bytes.Buffer
	// A length prefix claiming 100 MiB — well over maxFrameSize.
	buf.Write([]byte{0x06, 0x40, 0x00, 0x00, 0x01})
	r := NewFrameReader(&buf)
	if _, _, err := r.ReadFrame(); err != ErrFrameTooLarge {
		t.Errorf("got %v, want ErrFrameTooLarge", err)
	}
}

// TestStylePackRoundTrip checks the Style bit-packing helpers cover
// every field independently.
func TestStylePackRoundTrip(t *testing.T) {
	cases := []struct {
		name      string
		attrs     uint32
		underline uint8
		fgIdx     uint16
		fgRGB     bool
		bgIdx     uint16
		bgRGB     bool
	}{
		{"plain", 0, UnderlineNone, 7, false, 0, false},
		{"bold-italic", AttrBold | AttrItalic, UnderlineNone, 9, false, 4, false},
		{"underline-curly", 0, UnderlineCurly, 15, false, 0, false},
		{"fg-rgb", 0, UnderlineNone, 0, true, 8, false},
		{"bg-rgb", 0, UnderlineNone, 12, false, 0, true},
		{"both-rgb", AttrReverse, UnderlineDashed, 0, true, 0, true},
		{"max-palette-indices", AttrFaint, UnderlineDouble, 511, false, 511, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			packed := PackStyle(c.attrs, c.underline, c.fgIdx, c.fgRGB, c.bgIdx, c.bgRGB)
			a, u, fi, fr, bi, br := UnpackStyle(packed)
			if a != c.attrs {
				t.Errorf("attrs: got %x want %x", a, c.attrs)
			}
			if u != c.underline {
				t.Errorf("underline: got %d want %d", u, c.underline)
			}
			if fr != c.fgRGB {
				t.Errorf("fgRGB: got %v want %v", fr, c.fgRGB)
			}
			if br != c.bgRGB {
				t.Errorf("bgRGB: got %v want %v", br, c.bgRGB)
			}
			if !c.fgRGB && fi != c.fgIdx {
				t.Errorf("fgIdx: got %d want %d", fi, c.fgIdx)
			}
			if !c.bgRGB && bi != c.bgIdx {
				t.Errorf("bgIdx: got %d want %d", bi, c.bgIdx)
			}
		})
	}
}

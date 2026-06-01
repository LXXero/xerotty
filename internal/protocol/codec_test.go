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
// every field independently, including the fgSet/bgSet flags that
// distinguish "no color" from "ANSI black (palette idx 0)".
func TestStylePackRoundTrip(t *testing.T) {
	cases := []struct {
		name      string
		attrs     uint32
		underline uint8
		fgSet     bool
		fgIdx     uint16
		fgRGB     bool
		bgSet     bool
		bgIdx     uint16
		bgRGB     bool
	}{
		{"plain-default-colors", 0, UnderlineNone, false, 0, false, false, 0, false},
		{"fg-palette-7", 0, UnderlineNone, true, 7, false, false, 0, false},
		{"fg-ansi-black-palette-0", 0, UnderlineNone, true, 0, false, false, 0, false},
		{"bg-ansi-black-palette-0", 0, UnderlineNone, false, 0, false, true, 0, false},
		{"bold-italic", AttrBold | AttrItalic, UnderlineNone, true, 9, false, true, 4, false},
		{"underline-curly", 0, UnderlineCurly, true, 15, false, false, 0, false},
		{"fg-rgb", 0, UnderlineNone, true, 0, true, false, 0, false},
		{"bg-rgb", 0, UnderlineNone, true, 12, false, true, 0, true},
		{"both-rgb", AttrReverse, UnderlineDashed, true, 0, true, true, 0, true},
		{"max-palette-indices", AttrFaint, UnderlineDouble, true, 511, false, true, 511, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			packed := PackStyle(c.attrs, c.underline, c.fgSet, c.fgIdx, c.fgRGB, c.bgSet, c.bgIdx, c.bgRGB)
			a, u, fs, fi, fr, bs, bi, br := UnpackStyle(packed)
			if a != c.attrs {
				t.Errorf("attrs: got %x want %x", a, c.attrs)
			}
			if u != c.underline {
				t.Errorf("underline: got %d want %d", u, c.underline)
			}
			if fs != c.fgSet {
				t.Errorf("fgSet: got %v want %v", fs, c.fgSet)
			}
			if bs != c.bgSet {
				t.Errorf("bgSet: got %v want %v", bs, c.bgSet)
			}
			if fr != c.fgRGB {
				t.Errorf("fgRGB: got %v want %v", fr, c.fgRGB)
			}
			if br != c.bgRGB {
				t.Errorf("bgRGB: got %v want %v", br, c.bgRGB)
			}
			if c.fgSet && !c.fgRGB && fi != c.fgIdx {
				t.Errorf("fgIdx: got %d want %d", fi, c.fgIdx)
			}
			if c.bgSet && !c.bgRGB && bi != c.bgIdx {
				t.Errorf("bgIdx: got %d want %d", bi, c.bgIdx)
			}
		})
	}
}

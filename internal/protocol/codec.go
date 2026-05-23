package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/tinylib/msgp/msgp"
)

// Frame is one wire frame: a type tag + a msgpack-encoded body. The
// wire layout is:
//
//   [u32 length BE][u8 type][msgpack body...]
//
// Length is the size of the (type + body) tail, not including the
// length prefix itself. Cap'd at maxFrameSize to bound memory use.
// Image paste is chunked (MsgInputImageChunk, ImageChunkSize per
// frame) so even multi-megabyte screenshots fit comfortably under
// this — no single frame needs to carry a whole image.
const maxFrameSize = 16 * 1024 * 1024 // 16 MiB

// ImageChunkSize is the per-frame payload size for chunked image
// paste. 1 MiB keeps each frame well under maxFrameSize while
// keeping the chunk count low for typical screenshots.
const ImageChunkSize = 1 * 1024 * 1024 // 1 MiB

// ErrFrameTooLarge is returned when a peer sends a frame whose length
// prefix exceeds maxFrameSize. The connection should be closed when
// this fires; either the peer is malicious or we have a bug.
var ErrFrameTooLarge = errors.New("protocol: frame exceeds maxFrameSize")

// Msg is the marshal half of any message struct in this package.
// Used by WriteFrame; the unmarshal side is the caller's
// responsibility (ReadFrame returns the raw body which the caller
// passes to a typed UnmarshalMsg).
type Msg interface {
	msgp.Marshaler
	msgp.Sizer
}

// WriteFrame writes one frame to w. The body buffer is reused
// across calls when the caller provides one via WriteFrameBuf —
// this plain form allocates per call which is fine for low-rate
// control messages.
func WriteFrame(w io.Writer, t MsgType, body Msg) error {
	buf, err := body.MarshalMsg(nil)
	if err != nil {
		return fmt.Errorf("protocol: marshal %T: %w", body, err)
	}
	return writeFrameRaw(w, t, buf)
}

// WriteFrameBuf is the lower-overhead form: caller owns a reusable
// byte slice (typically the daemon's per-connection write buffer)
// to amortize allocations. Returns the (possibly grown) buffer.
func WriteFrameBuf(w io.Writer, t MsgType, body Msg, buf []byte) ([]byte, error) {
	buf = buf[:0]
	buf, err := body.MarshalMsg(buf)
	if err != nil {
		return buf, fmt.Errorf("protocol: marshal %T: %w", body, err)
	}
	return buf, writeFrameRaw(w, t, buf)
}

func writeFrameRaw(w io.Writer, t MsgType, body []byte) error {
	// length = 1 byte type tag + body
	total := 1 + len(body)
	if total > maxFrameSize {
		return ErrFrameTooLarge
	}
	var header [5]byte
	binary.BigEndian.PutUint32(header[:4], uint32(total))
	header[4] = byte(t)
	// Two writes — header then body — relies on the underlying
	// connection buffering them together. For unix sockets / TCP
	// this is fine; the OS coalesces small back-to-back writes.
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	_, err := w.Write(body)
	return err
}

// FrameReader reads frames from r incrementally. Caller invokes
// ReadFrame in a loop; the returned body buffer is reused across
// calls (callers must finish processing before calling ReadFrame
// again).
type FrameReader struct {
	r   io.Reader
	buf []byte // reused body buffer
}

// NewFrameReader wraps r. Call ReadFrame repeatedly.
func NewFrameReader(r io.Reader) *FrameReader {
	return &FrameReader{r: r}
}

// ReadFrame reads one frame from the underlying reader and returns
// its type + body. The body slice is owned by the reader and is
// only valid until the next ReadFrame call. Callers must Unmarshal
// before issuing the next read.
//
// Returns io.EOF cleanly if the connection closed at a frame
// boundary; any other error means the stream is corrupt or the
// peer crashed mid-frame.
func (fr *FrameReader) ReadFrame() (MsgType, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(fr.r, header[:]); err != nil {
		return 0, nil, err
	}
	length := binary.BigEndian.Uint32(header[:4])
	if length > maxFrameSize {
		return 0, nil, ErrFrameTooLarge
	}
	if length < 1 {
		// minimum valid frame: 1 byte type, 0 byte body. zero
		// length is invalid because it means we didn't even
		// receive the type.
		return 0, nil, fmt.Errorf("protocol: zero-length frame")
	}
	t := MsgType(header[4])
	bodyLen := int(length - 1)
	if cap(fr.buf) < bodyLen {
		fr.buf = make([]byte, bodyLen)
	} else {
		fr.buf = fr.buf[:bodyLen]
	}
	if bodyLen > 0 {
		if _, err := io.ReadFull(fr.r, fr.buf); err != nil {
			return 0, nil, err
		}
	}
	return t, fr.buf, nil
}

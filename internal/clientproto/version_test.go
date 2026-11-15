package clientproto

import (
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/LXXero/xerotty/internal/protocol"
)

// serveV10 plays a version-10 daemon's handshake on one connection:
// any other version gets the refusal a real version-10 build sends and
// the connection is closed.
func serveV10(conn net.Conn) {
	defer conn.Close()
	fr := protocol.NewFrameReader(conn)
	t, body, err := fr.ReadFrame()
	if err != nil || t != protocol.MsgHello {
		return
	}
	var hello protocol.Hello
	if _, err := hello.UnmarshalMsg(body); err != nil {
		return
	}
	if hello.Version != 10 {
		_ = protocol.WriteFrame(conn, protocol.MsgError, &protocol.Error{
			Code:    1,
			Message: fmt.Sprintf("unsupported protocol version %d; daemon speaks %d", hello.Version, 10),
		})
		return
	}
	_ = protocol.WriteFrame(conn, protocol.MsgHelloAck, &protocol.HelloAck{ServerVersion: 10})
	_, _, _ = fr.ReadFrame() // hold the connection until the client closes
}

// TestHelloFallsBackToVersion10 is the client half of an 11 client
// against a version-10 daemon: the first Hello is refused with the
// daemon's version, and a second connection speaking 10 is accepted.
func TestHelloFallsBackToVersion10(t *testing.T) {
	if protocol.ProtocolVersion != 11 || protocol.MinProtocolVersion != 10 {
		t.Skip("written for the 10/11 version pair")
	}
	a, b := net.Pipe()
	go serveV10(b)
	c := Wrap(a)
	_, err := c.Hello("new-client")
	_ = c.Close()
	var refused *VersionRefusedError
	if !errors.As(fmt.Errorf("hello: %w", err), &refused) {
		t.Fatalf("Hello against a version-10 daemon: err = %v, want VersionRefusedError", err)
	}
	if refused.Server != 10 || !refused.Retryable() {
		t.Fatalf("refused = %+v, retryable %v; want server 10, retryable", refused, refused.Retryable())
	}

	a, b = net.Pipe()
	go serveV10(b)
	c = Wrap(a)
	defer c.Close()
	if _, err := c.HelloVersion("new-client", refused.Server); err != nil {
		t.Fatalf("HelloVersion(10): %v", err)
	}
	if c.Version() != 10 {
		t.Fatalf("Version() = %d, want 10", c.Version())
	}

	// A daemon too old to adapt to is not retryable.
	if (&VersionRefusedError{Server: 9}).Retryable() {
		t.Fatal("version 9 must not be retryable")
	}
}

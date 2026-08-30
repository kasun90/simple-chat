package ws

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// RFC 6455 §1.3 worked example.
func TestAcceptKeyRFCVector(t *testing.T) {
	got := AcceptKey("dGhlIHNhbXBsZSBub25jZQ==")
	if want := "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="; got != want {
		t.Fatalf("AcceptKey = %q, want %q", got, want)
	}
}

// clientFrame builds a masked client→server frame the way a browser would.
func clientFrame(op Opcode, payload []byte) []byte {
	key := [4]byte{0x12, 0x34, 0x56, 0x78}
	var b bytes.Buffer
	b.WriteByte(0x80 | byte(op))
	n := len(payload)
	switch {
	case n < 126:
		b.WriteByte(0x80 | byte(n))
	case n <= 0xFFFF:
		b.WriteByte(0x80 | 126)
		_ = binary.Write(&b, binary.BigEndian, uint16(n))
	default:
		b.WriteByte(0x80 | 127)
		_ = binary.Write(&b, binary.BigEndian, uint64(n))
	}
	b.Write(key[:])
	masked := append([]byte(nil), payload...)
	mask(masked, key)
	b.Write(masked)
	return b.Bytes()
}

// Covers the 7-bit, 16-bit and 64-bit length encodings on both paths.
func TestFrameRoundTripAllLengthForms(t *testing.T) {
	for _, n := range []int{0, 5, 125, 126, 300, 65535, 65536, 100000} {
		payload := bytes.Repeat([]byte{'x'}, n)

		f, err := readFrame(bytes.NewReader(clientFrame(OpText, payload)), 1<<20)
		if err != nil {
			t.Fatalf("n=%d read: %v", n, err)
		}
		if !f.fin || f.opcode != OpText || !bytes.Equal(f.payload, payload) {
			t.Fatalf("n=%d decoded frame mismatch", n)
		}

		var out bytes.Buffer
		if err := writeFrame(&out, OpText, payload); err != nil {
			t.Fatal(err)
		}
		// Server frames are unmasked; check header shape then payload.
		hdr := out.Bytes()
		if hdr[0] != 0x81 {
			t.Fatalf("n=%d bad first byte %x", n, hdr[0])
		}
		if hdr[1]&0x80 != 0 {
			t.Fatalf("n=%d server frame must not be masked", n)
		}
		if !bytes.HasSuffix(out.Bytes(), payload) {
			t.Fatalf("n=%d payload not written", n)
		}
	}
}

func TestReadFrameRejectsOversized(t *testing.T) {
	_, err := readFrame(bytes.NewReader(clientFrame(OpText, make([]byte, 200))), 100)
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("want ErrFrameTooLarge, got %v", err)
	}
	// A hostile length field must be refused before any allocation happens:
	// header claims 2^40 bytes but sends none.
	hdr := []byte{0x81, 0x80 | 127, 0, 0, 0x01, 0, 0, 0, 0, 0}
	_, err = readFrame(bytes.NewReader(hdr), DefaultMaxPayload)
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("want ErrFrameTooLarge for hostile length, got %v", err)
	}
}

func TestReadFrameRejectsUnmaskedAndRSV(t *testing.T) {
	unmasked := []byte{0x81, 0x02, 'h', 'i'}
	if _, err := readFrame(bytes.NewReader(unmasked), 1024); !errors.Is(err, ErrProtocol) {
		t.Fatalf("unmasked: want ErrProtocol, got %v", err)
	}
	rsv := clientFrame(OpText, []byte("hi"))
	rsv[0] |= 0x40
	if _, err := readFrame(bytes.NewReader(rsv), 1024); !errors.Is(err, ErrProtocol) {
		t.Fatalf("rsv: want ErrProtocol, got %v", err)
	}
}

// dial performs a client handshake against an httptest server and returns
// the raw connection plus a reader positioned after the 101 response.
func dial(t *testing.T, srv *httptest.Server) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	req := "GET /ws HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: keep-alive, Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("bad accept header %q", got)
	}
	return conn, br
}

// readServerFrame decodes an unmasked server→client frame.
func readServerFrame(t *testing.T, r io.Reader) (Opcode, []byte) {
	t.Helper()
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		t.Fatal(err)
	}
	n := int(hdr[1] & 0x7F)
	if n == 126 {
		var ext [2]byte
		_, _ = io.ReadFull(r, ext[:])
		n = int(binary.BigEndian.Uint16(ext[:]))
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		t.Fatal(err)
	}
	return Opcode(hdr[0] & 0x0F), p
}

func TestUpgradeEchoPingClose(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Upgrade(w, r)
		if err != nil {
			return
		}
		for {
			msg, err := c.ReadText()
			if err != nil {
				return
			}
			_ = c.WriteText("echo:" + msg)
		}
	}))
	defer srv.Close()

	conn, br := dial(t, srv)
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	// Text is echoed.
	_, _ = conn.Write(clientFrame(OpText, []byte("hello")))
	if op, p := readServerFrame(t, br); op != OpText || string(p) != "echo:hello" {
		t.Fatalf("got %v %q", op, p)
	}
	// Ping is answered with a pong carrying the same payload, transparently.
	_, _ = conn.Write(clientFrame(OpPing, []byte("p1")))
	if op, p := readServerFrame(t, br); op != OpPong || string(p) != "p1" {
		t.Fatalf("got %v %q", op, p)
	}
	// Close is echoed and the socket is dropped.
	closeBody := []byte{0x03, 0xE8} // 1000
	_, _ = conn.Write(clientFrame(OpClose, closeBody))
	if op, p := readServerFrame(t, br); op != OpClose || !bytes.Equal(p, closeBody) {
		t.Fatalf("got %v %v", op, p)
	}
	if _, err := br.ReadByte(); err == nil {
		t.Fatal("expected EOF after close")
	}
}

func TestUpgradeRejectsPlainGET(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := Upgrade(w, r); err == nil {
			t.Error("expected upgrade failure")
		}
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestBinaryFrameFailsConnection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Upgrade(w, r)
		if err != nil {
			return
		}
		_, _ = c.ReadText()
	}))
	defer srv.Close()
	conn, br := dial(t, srv)
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = conn.Write(clientFrame(OpBinary, []byte{1, 2, 3}))
	op, p := readServerFrame(t, br)
	if op != OpClose || binary.BigEndian.Uint16(p[:2]) != CloseUnsupportedData {
		t.Fatalf("got %v %v", op, p)
	}
}

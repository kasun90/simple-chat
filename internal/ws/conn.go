package ws

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DefaultMaxPayload caps a single incoming frame. Chat messages are short; a
// 64 KiB limit is generous for text and small enough that a malicious client
// cannot make the server allocate gigabytes.
const DefaultMaxPayload = 64 << 10

// ErrClosed is returned from Read once the peer has closed the connection.
var ErrClosed = errors.New("ws: connection closed")

// Conn is one WebSocket connection. It is safe for one reader goroutine and
// any number of writer goroutines: writes are serialised by a mutex because
// two frames written concurrently to the same TCP stream would interleave
// bytes and corrupt both.
type Conn struct {
	conn       net.Conn
	br         *bufio.Reader
	writeMu    sync.Mutex
	maxPayload int64
	// ReadTimeout is applied before every read. Together with periodic pings
	// (see KeepAlive) it detects half-open connections — e.g. a laptop lid
	// closed — which otherwise never error and would leak forever.
	ReadTimeout time.Duration
	closeOnce   sync.Once
}

// Upgrade performs the opening handshake (§4.2.2) and takes over the TCP
// connection. It writes an HTTP error and returns an error if the request is
// not a valid WebSocket upgrade.
func Upgrade(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	if r.Method != http.MethodGet ||
		!headerContainsToken(r.Header, "Connection", "upgrade") ||
		!strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
		r.Header.Get("Sec-WebSocket-Version") != "13" {
		http.Error(w, "expected WebSocket upgrade", http.StatusBadRequest)
		return nil, ErrProtocol
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "missing Sec-WebSocket-Key", http.StatusBadRequest)
		return nil, ErrProtocol
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "server does not support hijacking", http.StatusInternalServerError)
		return nil, errors.New("ws: response writer is not a Hijacker")
	}
	netConn, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}

	// The 101 response is written by hand because after Hijack the
	// http.Server no longer owns the connection.
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + AcceptKey(key) + "\r\n\r\n"
	if _, err := netConn.Write([]byte(resp)); err != nil {
		netConn.Close()
		return nil, err
	}

	return &Conn{
		conn:       netConn,
		br:         rw.Reader, // may already hold bytes the client sent early
		maxPayload: DefaultMaxPayload,
	}, nil
}

// AcceptKey derives Sec-WebSocket-Accept from Sec-WebSocket-Key (§4.2.2).
// The fixed GUID is defined by the RFC; it exists purely so that a non-WS
// HTTP server cannot accidentally produce a valid-looking response.
func AcceptKey(clientKey string) string {
	h := sha1.New()
	h.Write([]byte(clientKey + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func headerContainsToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// ReadText blocks until a complete text message arrives. Control frames are
// handled internally: pings are answered, pongs are ignored, close triggers
// the closing handshake and returns ErrClosed. Anything the chat protocol does
// not use (binary, fragmentation) fails the connection per §5.
func (c *Conn) ReadText() (string, error) {
	for {
		if c.ReadTimeout > 0 {
			_ = c.conn.SetReadDeadline(time.Now().Add(c.ReadTimeout))
		}
		f, err := readFrame(c.br, c.maxPayload)
		if err != nil {
			switch {
			case errors.Is(err, ErrFrameTooLarge):
				c.Close(CloseMessageTooBig, "frame too large")
			case errors.Is(err, ErrProtocol):
				c.Close(CloseProtocolError, err.Error())
			default:
				c.closeConn() // network error / deadline: no point in a handshake
			}
			return "", err
		}
		switch f.opcode {
		case OpText:
			if !f.fin {
				c.Close(CloseProtocolError, "fragmentation not supported")
				return "", ErrProtocol
			}
			return string(f.payload), nil
		case OpPing:
			// Pong must echo the ping's payload (§5.5.3).
			if err := c.write(OpPong, f.payload); err != nil {
				return "", err
			}
		case OpPong:
			// Reply to our KeepAlive ping; the read deadline reset above is
			// the only side effect we need.
		case OpClose:
			// Echo the close so the peer can finish its handshake, then drop.
			_ = c.write(OpClose, f.payload)
			c.closeConn()
			return "", ErrClosed
		default:
			c.Close(CloseUnsupportedData, "only text frames are supported")
			return "", ErrProtocol
		}
	}
}

// WriteText sends one text message.
func (c *Conn) WriteText(s string) error { return c.write(OpText, []byte(s)) }

// Ping sends a ping; the peer's pong resets the read deadline in ReadText.
func (c *Conn) Ping() error { return c.write(OpPing, nil) }

func (c *Conn) write(op Opcode, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	// A stuck client must not block the whole hub forever; bound each write.
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	err := writeFrame(c.conn, op, payload)
	if err != nil {
		// A failed write means the peer is gone or wedged. Dropping the
		// socket here makes the reader fail too, so whoever owns the
		// connection notices immediately instead of on the next timeout.
		c.closeConn()
	}
	return err
}

// Close sends a close frame with a status code and reason, then closes the
// socket. Safe to call more than once.
func (c *Conn) Close(code uint16, reason string) {
	payload := make([]byte, 2, 2+len(reason))
	binary.BigEndian.PutUint16(payload, code)
	payload = append(payload, reason...)
	_ = c.write(OpClose, payload)
	c.closeConn()
}

func (c *Conn) closeConn() {
	c.closeOnce.Do(func() { _ = c.conn.Close() })
}

// KeepAlive pings every interval until the connection is closed. Run it in
// its own goroutine. ReadTimeout should be comfortably larger than interval
// so a single delayed pong does not drop a healthy connection.
func (c *Conn) KeepAlive(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for range t.C {
		if err := c.Ping(); err != nil {
			return
		}
	}
}

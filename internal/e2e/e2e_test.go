// Package e2e drives the assembled server the way a browser would: HTTP
// login for a cookie, a real WebSocket handshake, JSON frames both ways.
// It exists so the delivery guarantees (#8) are tested end to end rather
// than inferred from unit tests of each layer.
package e2e

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kasun90/simple-chat/internal/app"
	"github.com/kasun90/simple-chat/internal/hub"
	"github.com/kasun90/simple-chat/internal/store"
)

// wsClient is a deliberately tiny browser stand-in.
type wsClient struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
}

func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	handler, err := app.New(st, []string{"alice", "bob"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func login(t *testing.T, srv *httptest.Server, name string) (store.User, *http.Cookie) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/api/login", "application/json", strings.NewReader(`{"username":"`+name+`"}`))
	if err != nil {
		t.Fatalf("login %s: %v", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("login %s: status %d", name, resp.StatusCode)
	}
	var u store.User
	_ = json.NewDecoder(resp.Body).Decode(&u)
	for _, c := range resp.Cookies() {
		if c.Name == "chat_session" {
			return u, c
		}
	}
	t.Fatal("no session cookie")
	return u, nil
}

func dialWS(t *testing.T, srv *httptest.Server, cookie *http.Cookie, origin string) *wsClient {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	req := "GET /ws HTTP/1.1\r\nHost: " + strings.TrimPrefix(srv.URL, "http://") + "\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"
	if cookie != nil {
		req += "Cookie: " + cookie.Name + "=" + cookie.Value + "\r\n"
	}
	if origin != "" {
		req += "Origin: " + origin + "\r\n"
	}
	_, _ = conn.Write([]byte(req + "\r\n"))
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 101 {
		conn.Close()
		t.Fatalf("upgrade status %d", resp.StatusCode)
	}
	c := &wsClient{t: t, conn: conn, br: br}
	t.Cleanup(func() { conn.Close() })
	return c
}

func upgradeStatus(t *testing.T, srv *httptest.Server, cookie *http.Cookie, origin string) int {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+"/ws", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func (c *wsClient) send(v any) {
	payload, _ := json.Marshal(v)
	key := [4]byte{1, 2, 3, 4}
	frame := []byte{0x81}
	n := len(payload)
	switch {
	case n < 126:
		frame = append(frame, 0x80|byte(n))
	default:
		frame = append(frame, 0x80|126, byte(n>>8), byte(n))
	}
	frame = append(frame, key[:]...)
	for i, b := range payload {
		frame = append(frame, b^key[i%4])
	}
	if _, err := c.conn.Write(frame); err != nil {
		c.t.Fatal(err)
	}
}

// recv returns the next event of the wanted type, skipping others.
func (c *wsClient) recv(want string) hub.Outbound {
	c.t.Helper()
	for {
		var hdr [2]byte
		if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
			c.t.Fatalf("read: %v", err)
		}
		n := int(hdr[1] & 0x7F)
		if n == 126 {
			var ext [2]byte
			_, _ = io.ReadFull(c.br, ext[:])
			n = int(binary.BigEndian.Uint16(ext[:]))
		}
		p := make([]byte, n)
		_, _ = io.ReadFull(c.br, p)
		if hdr[0]&0x0F != 0x1 {
			continue // ping etc.
		}
		var out hub.Outbound
		if err := json.Unmarshal(p, &out); err != nil {
			c.t.Fatal(err)
		}
		if out.Type == want {
			return out
		}
	}
}

func TestSendReceiveDuplicateAndResync(t *testing.T) {
	srv := newServer(t)
	alice, aCookie := login(t, srv, "alice")
	bob, bCookie := login(t, srv, "bob")
	a := dialWS(t, srv, aCookie, "")
	b := dialWS(t, srv, bCookie, "")

	// 1. Real-time delivery.
	a.send(hub.Inbound{Type: "send", To: bob.ID, Body: "hello bob", ClientMsgID: "k1"})
	got := b.recv("message")
	if got.Message.Body != "hello bob" || got.Message.SenderID != alice.ID {
		t.Fatalf("bob got %+v", got.Message)
	}
	echo := a.recv("message")
	if echo.Message.ID != got.Message.ID {
		t.Fatal("sender echo must carry the same server ID")
	}

	// 2. Retry with the same clientMsgId returns the same ID to alice only.
	a.send(hub.Inbound{Type: "send", To: bob.ID, Body: "hello bob", ClientMsgID: "k1"})
	if again := a.recv("message"); again.Message.ID != echo.Message.ID {
		t.Fatal("duplicate must map to the original message")
	}

	// 3. Bob goes offline; alice keeps talking; bob resyncs by cursor.
	b.conn.Close()
	a.recv("presence") // bob offline
	a.send(hub.Inbound{Type: "send", To: bob.ID, Body: "while you were away", ClientMsgID: "k2"})
	a.recv("message")

	req, _ := http.NewRequest("GET", srv.URL+"/api/conversations/"+itoa(alice.ID)+"/messages?after="+itoa(got.Message.ID), nil)
	req.AddCookie(bCookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var missed []store.Message
	_ = json.NewDecoder(resp.Body).Decode(&missed)
	if len(missed) != 1 || missed[0].Body != "while you were away" {
		t.Fatalf("resync returned %+v", missed)
	}
}

func TestUpgradeRequiresSessionAndSameOrigin(t *testing.T) {
	srv := newServer(t)
	_, cookie := login(t, srv, "alice")
	if s := upgradeStatus(t, srv, nil, ""); s != 401 {
		t.Fatalf("no cookie: %d", s)
	}
	if s := upgradeStatus(t, srv, cookie, "http://evil.example"); s != 403 {
		t.Fatalf("foreign origin: %d", s)
	}
	dialWS(t, srv, cookie, srv.URL) // same origin: succeeds (fails the test otherwise)
}

func TestFrontendIsServed(t *testing.T) {
	srv := newServer(t)
	for _, p := range []string{"/", "/app.js", "/style.css", "/healthz"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s: status %d", p, resp.StatusCode)
		}
	}
}

func itoa(i int64) string { return strconv.FormatInt(i, 10) }

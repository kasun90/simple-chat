// Package hub routes messages between connected users in real time.
//
// A Hub holds every open socket in memory keyed by user ID. That is the
// simplest thing that works for one process; OBSTACLES.md explains why it is
// the first component to change when the server runs as several replicas.
package hub

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"slices"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/kasun90/simple-chat/internal/store"
)

// Socket is what the hub needs from a connection. ws.Conn satisfies it; tests
// use an in-memory fake so routing logic is tested without TCP.
type Socket interface {
	ReadText() (string, error)
	WriteText(string) error
	Close(code uint16, reason string)
}

// MaxBodyRunes bounds a chat message. Counted in runes, not bytes, so a
// non-Latin user gets the same limit as an English one.
const MaxBodyRunes = 2000

// sendBuffer is how many outbound messages may queue per socket before the
// socket is considered dead. Blocking instead would let one stalled browser
// tab hold the hub lock and freeze every other user.
const sendBuffer = 64

// Inbound is what a client sends over the socket.
type Inbound struct {
	Type        string `json:"type"` // "send"
	To          int64  `json:"to"`
	GroupID     int64  `json:"groupID"`
	Body        string `json:"body"`
	ClientMsgID string `json:"clientMsgId"`
}

// Outbound is what the server pushes. Exactly one of the optional fields is
// set depending on Type:
//   - "message":  Message (a new chat message, delivered to both parties)
//   - "presence": UserID + Online (someone connected/disconnected)
//   - "snapshot": Online (IDs of everyone currently connected; sent on join)
//   - "error":    Error (the previous inbound frame was rejected)
type Outbound struct {
	Type      string         `json:"type"`
	Message   *store.Message `json:"message,omitempty"`
	UserID    int64          `json:"userId,omitempty"`
	Online    *bool          `json:"online,omitempty"`
	OnlineIDs []int64        `json:"onlineIds,omitempty"`
	Error     string         `json:"error,omitempty"`
}

type client struct {
	userID int64
	sock   Socket
	send   chan string
}

type Hub struct {
	store store.Store
	mu    sync.Mutex
	// conns is userID → the set of that user's open sockets. A user with two
	// tabs has two entries and both must receive every event.
	conns map[int64]map[*client]struct{}
}

func New(st store.Store) *Hub {
	return &Hub{store: st, conns: make(map[int64]map[*client]struct{})}
}

// Serve runs the connection until the socket errors or closes. It must be
// called from the HTTP handler goroutine; it blocks for the socket's life.
func (h *Hub) Serve(ctx context.Context, userID int64, sock Socket) {
	c := &client{userID: userID, sock: sock, send: make(chan string, sendBuffer)}
	h.register(c)
	defer h.unregister(c)

	// One writer goroutine per socket drains the channel. The hub only ever
	// does a non-blocking channel send under its lock, so a slow socket
	// cannot stall the hub — it just fills its own buffer and gets dropped.
	go func() {
		for msg := range c.send {
			if err := sock.WriteText(msg); err != nil {
				// Closing makes ReadText fail so Serve returns and unregister
				// runs. Without this the client would stay registered with
				// nobody draining its buffer, and messages would silently
				// stall until the buffer filled.
				sock.Close(1001, "write failed")
				return
			}
		}
	}()

	for {
		raw, err := sock.ReadText()
		if err != nil {
			return
		}
		var in Inbound
		if err := json.Unmarshal([]byte(raw), &in); err != nil {
			c.push(Outbound{Type: "error", Error: "invalid json"})
			continue
		}
		if msg := h.handleInbound(ctx, userID, in); msg != "" {
			c.push(Outbound{Type: "error", Error: msg})
		}
	}
}

// handleInbound validates, persists, then fans out. It returns a user-facing
// error string, or "" on success.
func (h *Hub) handleInbound(ctx context.Context, from int64, in Inbound) string {
	if in.Type != "send" {
		return "unknown message type"
	}

	if in.To == 0 || in.GroupID == 0 {
		return "should be a recipient or a group"
	}

	if in.Body == "" || utf8.RuneCountInString(in.Body) > MaxBodyRunes {
		return "body must be 1-2000 characters"
	}

	if in.ClientMsgID == "" || len(in.ClientMsgID) > 64 {
		return "clientMsgId required"
	}

	if in.GroupID != 0 {
		return h.sendToGroup(ctx, from, in)
	}

	// 1:1 flow stays same
	if in.To == from {
		return "invalid recipient"
	}

	if _, err := h.store.GetUser(ctx, in.To); err != nil {
		return "unknown recipient"
	}

	// Persist BEFORE fan-out. If the order were reversed and the process
	// died in between, the recipient would have seen a message that does not
	// exist in history — the worst kind of inconsistency for a chat app.
	msg, duplicate, err := h.store.AppendMessage(ctx, from, in.To, in.Body, in.ClientMsgID)
	if errors.Is(err, store.ErrClientMsgIDReused) {
		return "clientMsgId already used for another conversation"
	}
	if err != nil {
		log.Printf("append message: %v", err)
		return "could not store message"
	}
	if duplicate {
		// A retry after a lost ack: the recipient already has it. Re-deliver
		// only to the sender so its UI can clear the pending state.
		h.broadcast(Outbound{Type: "message", Message: &msg}, from)
		return ""
	}
	h.broadcast(Outbound{Type: "message", Message: &msg}, from, in.To)
	return ""
}

func (h *Hub) sendToGroup(ctx context.Context, from int64, in Inbound) string {
	members, err := h.store.GroupMembers(ctx, from, in.GroupID)
	if err != nil {
		return "couldnt fetch members"
	}

	if !slices.Contains(members, from) {
		return "not a member in this group"
	}

	msg, duplicate, err := h.store.AppendGroupMessage(ctx, from, in.GroupID, in.Body, in.ClientMsgID)
	if errors.Is(err, store.ErrClientMsgIDReused) {
		return "clientMsgId already used for another conversation"
	}

	if err != nil {
		log.Printf("append message: %v", err)
		return "could not store message"
	}

	if duplicate {
		// A retry after a lost ack: the recipient already has it. Re-deliver
		// only to the sender so its UI can clear the pending state.
		h.broadcast(Outbound{Type: "message", Message: &msg}, from)
		return ""
	}

	h.broadcast(Outbound{Type: "message", Message: &msg}, members...)
	return ""
}

// broadcast delivers an event to every socket of each listed user. The
// sender is included on purpose: its other tabs/devices need the message,
// and the originating tab uses the echoed clientMsgId to mark it delivered.
func (h *Hub) broadcast(out Outbound, userIDs ...int64) {
	payload := encode(out)
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, id := range userIDs {
		for c := range h.conns[id] {
			c.pushRaw(payload)
		}
	}
}

func (h *Hub) register(c *client) {
	h.mu.Lock()
	first := len(h.conns[c.userID]) == 0
	if h.conns[c.userID] == nil {
		h.conns[c.userID] = make(map[*client]struct{})
	}
	h.conns[c.userID][c] = struct{}{}
	online := make([]int64, 0, len(h.conns))
	for id := range h.conns {
		online = append(online, id)
	}
	// Tell the newcomer who is online, then (if this is their first socket)
	// tell everyone else they arrived. Done under the lock so no presence
	// change can slip between the snapshot and the subscription.
	c.pushRaw(encode(Outbound{Type: "snapshot", OnlineIDs: online}))
	if first {
		h.broadcastLocked(Outbound{Type: "presence", UserID: c.userID, Online: ptr(true)}, c.userID)
	}
	h.mu.Unlock()
}

func (h *Hub) unregister(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.conns[c.userID]
	if _, ok := set[c]; !ok {
		return
	}
	delete(set, c)
	close(c.send) // stops the writer goroutine
	if len(set) == 0 {
		delete(h.conns, c.userID)
		h.broadcastLocked(Outbound{Type: "presence", UserID: c.userID, Online: ptr(false)}, c.userID)
	}
}

// broadcastLocked sends to everyone except the given user (their own
// presence is not news to them); caller holds h.mu.
func (h *Hub) broadcastLocked(out Outbound, except int64) {
	payload := encode(out)
	for id, set := range h.conns {
		if id == except {
			continue
		}
		for c := range set {
			c.pushRaw(payload)
		}
	}
}

// Online reports whether the user has at least one open socket.
func (h *Hub) Online(userID int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.conns[userID]) > 0
}

func (c *client) push(out Outbound) { c.pushRaw(encode(out)) }

// pushRaw is non-blocking. A full buffer means the client is not reading;
// closing the socket makes its read loop exit and unregister run. Blocking
// here instead would deadlock the hub behind one bad connection.
func (c *client) pushRaw(payload string) {
	select {
	case c.send <- payload:
	default:
		go c.sock.Close(1008, "client too slow")
	}
}

func encode(out Outbound) string {
	b, err := json.Marshal(out)
	if err != nil {
		panic(err) // Outbound only holds plain data; this cannot fail
	}
	return string(b)
}

func ptr(b bool) *bool { return &b }

// KeepAliveInterval / ReadTimeout tune dead-connection detection. ReadTimeout
// is 2× the interval so one delayed pong does not drop a healthy socket.
const (
	KeepAliveInterval = 30 * time.Second
	ReadTimeout       = 2 * KeepAliveInterval
)

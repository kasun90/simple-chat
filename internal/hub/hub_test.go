package hub

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kasun90/simple-chat/internal/store"
)

// fakeSocket feeds inbound frames from `in` and records outbound in `out`.
type fakeSocket struct {
	in     chan string
	out    chan string
	closed chan struct{}
	// snapshot is the event received on registration; connect() waits for it
	// so a test never races ahead of Serve's register call.
	snapshot Outbound
}

func newFake() *fakeSocket {
	return &fakeSocket{in: make(chan string), out: make(chan string, 100), closed: make(chan struct{})}
}
func (f *fakeSocket) ReadText() (string, error) {
	select {
	case s := <-f.in:
		return s, nil
	case <-f.closed:
		return "", context.Canceled
	}
}
func (f *fakeSocket) WriteText(s string) error { f.out <- s; return nil }
func (f *fakeSocket) Close(uint16, string) {
	select {
	case <-f.closed:
	default:
		close(f.closed)
	}
}

// next returns the next outbound event of the given type, skipping others.
func (f *fakeSocket) next(t *testing.T, typ string) Outbound {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case raw := <-f.out:
			var o Outbound
			if err := json.Unmarshal([]byte(raw), &o); err != nil {
				t.Fatal(err)
			}
			if o.Type == typ {
				return o
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %q", typ)
		}
	}
}

func (f *fakeSocket) expectNone(t *testing.T) {
	t.Helper()
	select {
	case raw := <-f.out:
		t.Fatalf("unexpected event: %s", raw)
	case <-time.After(50 * time.Millisecond):
	}
}

func setup(t *testing.T) (*Hub, store.User, store.User) {
	t.Helper()
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	alice, _ := st.UpsertUser(ctx, "alice")
	bob, _ := st.UpsertUser(ctx, "bob")
	return New(st), alice, bob
}

func connect(t *testing.T, h *Hub, userID int64) *fakeSocket {
	t.Helper()
	f := newFake()
	go h.Serve(context.Background(), userID, f)
	f.snapshot = f.next(t, "snapshot")
	return f
}

func TestMessageReachesRecipientAndAllSenderTabs(t *testing.T) {
	h, alice, bob := setup(t)
	a1, a2, b1 := connect(t, h, alice.ID), connect(t, h, alice.ID), connect(t, h, bob.ID)
	defer a1.Close(0, "")
	defer a2.Close(0, "")
	defer b1.Close(0, "")

	a1.in <- `{"type":"send","to":` + itoa(bob.ID) + `,"body":"hello","clientMsgId":"c1"}`

	for _, s := range []*fakeSocket{a1, a2, b1} {
		m := s.next(t, "message")
		if m.Message.Body != "hello" || m.Message.ClientMsgID != "c1" || m.Message.SenderID != alice.ID {
			t.Fatalf("bad message %+v", m.Message)
		}
	}
}

func TestDuplicateClientMsgIDNotRedeliveredToRecipient(t *testing.T) {
	h, alice, bob := setup(t)
	a1, b1 := connect(t, h, alice.ID), connect(t, h, bob.ID)
	defer a1.Close(0, "")
	defer b1.Close(0, "")

	frame := `{"type":"send","to":` + itoa(bob.ID) + `,"body":"once","clientMsgId":"dup"}`
	a1.in <- frame
	first := a1.next(t, "message")
	b1.next(t, "message")

	a1.in <- frame // retry after a "lost ack"
	again := a1.next(t, "message")
	if again.Message.ID != first.Message.ID {
		t.Fatal("retry should echo the original message ID")
	}
	b1.expectNone(t)
}

func TestValidationErrors(t *testing.T) {
	h, alice, bob := setup(t)
	a1 := connect(t, h, alice.ID)
	defer a1.Close(0, "")

	cases := []string{
		`garbage`,
		`{"type":"nope"}`,
		`{"type":"send","to":` + itoa(alice.ID) + `,"body":"me","clientMsgId":"x"}`, // self
		`{"type":"send","to":999,"body":"ghost","clientMsgId":"x"}`,                 // unknown user
		`{"type":"send","to":` + itoa(bob.ID) + `,"body":"","clientMsgId":"x"}`,     // empty body
		`{"type":"send","to":` + itoa(bob.ID) + `,"body":"` + strings.Repeat("é", MaxBodyRunes+1) + `","clientMsgId":"x"}`,
		`{"type":"send","to":` + itoa(bob.ID) + `,"body":"hi"}`, // missing clientMsgId
	}
	for _, c := range cases {
		a1.in <- c
		if e := a1.next(t, "error"); e.Error == "" {
			t.Fatalf("expected error for %s", c)
		}
	}
}

func TestPresenceAndSnapshot(t *testing.T) {
	h, alice, bob := setup(t)
	a1 := connect(t, h, alice.ID)
	defer a1.Close(0, "")
	snap := a1.snapshot
	if len(snap.OnlineIDs) != 1 || snap.OnlineIDs[0] != alice.ID {
		t.Fatalf("snapshot %+v", snap.OnlineIDs)
	}

	b1 := connect(t, h, bob.ID)
	p := a1.next(t, "presence")
	if p.UserID != bob.ID || !*p.Online {
		t.Fatalf("expected bob online, got %+v", p)
	}
	b2 := connect(t, h, bob.ID) // second tab: no new presence event
	a1.expectNone(t)

	b1.Close(0, "")
	a1.expectNone(t) // still one bob tab open
	b2.Close(0, "")
	p = a1.next(t, "presence")
	if p.UserID != bob.ID || *p.Online {
		t.Fatalf("expected bob offline, got %+v", p)
	}
	if h.Online(bob.ID) || !h.Online(alice.ID) {
		t.Fatal("Online() wrong")
	}
}

func TestSlowConsumerIsDisconnected(t *testing.T) {
	h, alice, bob := setup(t)
	// A socket whose writes block forever, simulating a stalled tab. `out`
	// has room for exactly one frame so the registration snapshot can be
	// observed (proving Bob is registered before Alice starts sending); every
	// write after that blocks the writer goroutine.
	stuck := &fakeSocket{in: make(chan string), out: make(chan string, 1), closed: make(chan struct{})}
	go h.Serve(context.Background(), bob.ID, stuck)
	stuck.next(t, "snapshot")
	a1 := connect(t, h, alice.ID)
	defer a1.Close(0, "")

	for i := 0; i < sendBuffer+5; i++ {
		a1.in <- `{"type":"send","to":` + itoa(bob.ID) + `,"body":"spam","clientMsgId":"k` + itoa(int64(i)) + `"}`
		a1.next(t, "message")
	}
	select {
	case <-stuck.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("slow consumer was not closed")
	}
}

func itoa(i int64) string { return strconv.FormatInt(i, 10) }

// failSocket errors on every write, like a peer whose TCP window never opens.
type failSocket struct{ *fakeSocket }

func (f failSocket) WriteText(string) error { return context.DeadlineExceeded }

func TestWriteFailureClosesSocket(t *testing.T) {
	h, alice, bob := setup(t)
	broken := failSocket{newFake()}
	go h.Serve(context.Background(), bob.ID, broken)
	select { // the snapshot write on register already fails
	case <-broken.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("socket with failing writes was not closed")
	}
	_ = alice
}

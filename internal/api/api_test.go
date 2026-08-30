package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/kasun90/simple-chat/internal/auth"
	"github.com/kasun90/simple-chat/internal/store"
)

type fakePresence map[int64]bool

func (f fakePresence) Online(id int64) bool { return f[id] }

type env struct {
	srv    *httptest.Server
	alice  store.User
	bob    store.User
	carol  store.User
	cookie *http.Cookie
}

func setup(t *testing.T) env {
	t.Helper()
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	alice, _ := st.UpsertUser(ctx, "alice")
	bob, _ := st.UpsertUser(ctx, "bob")
	carol, _ := st.UpsertUser(ctx, "carol")
	_, _, _ = st.AppendMessage(ctx, alice.ID, bob.ID, "m1", "c1")
	_, _, _ = st.AppendMessage(ctx, bob.ID, alice.ID, "m2", "c2")
	_, _, _ = st.AppendMessage(ctx, alice.ID, carol.ID, "secret", "c3")

	sessions := auth.NewSessions(time.Hour)
	mux := http.NewServeMux()
	(&Handlers{Store: st, Presence: fakePresence{bob.ID: true}}).Register(mux, sessions)
	srv := httptest.NewServer(Logging(mux))
	t.Cleanup(srv.Close)
	return env{srv, alice, bob, carol, &http.Cookie{Name: auth.CookieName, Value: sessions.Create(alice.ID)}}
}

func (e env) get(t *testing.T, path string, v any) int {
	t.Helper()
	req, _ := http.NewRequest("GET", e.srv.URL+path, nil)
	req.AddCookie(e.cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if v != nil && resp.StatusCode == 200 {
		_ = json.NewDecoder(resp.Body).Decode(v)
	}
	return resp.StatusCode
}

func TestListUsersExcludesSelfAndShowsPresence(t *testing.T) {
	e := setup(t)
	var users []contact
	if code := e.get(t, "/api/users", &users); code != 200 {
		t.Fatalf("status %d", code)
	}
	if len(users) != 2 || users[0].Username != "bob" || !users[0].Online || users[1].Online {
		t.Fatalf("unexpected users: %+v", users)
	}
}

func TestListMessagesWithCursor(t *testing.T) {
	e := setup(t)
	var msgs []store.Message
	e.get(t, "/api/conversations/"+itoa(e.bob.ID)+"/messages", &msgs)
	if len(msgs) != 2 || msgs[0].Body != "m1" {
		t.Fatalf("history: %+v", msgs)
	}
	e.get(t, "/api/conversations/"+itoa(e.bob.ID)+"/messages?after="+itoa(msgs[0].ID), &msgs)
	if len(msgs) != 1 || msgs[0].Body != "m2" {
		t.Fatalf("cursor: %+v", msgs)
	}
	// Alice cannot read the bob<->carol conversation by asking for bob's
	// messages: the caller is always one side of the pair.
	e.get(t, "/api/conversations/"+itoa(e.carol.ID)+"/messages", &msgs)
	if len(msgs) != 1 || msgs[0].Body != "secret" {
		t.Fatalf("alice<->carol: %+v", msgs)
	}
}

func TestListMessagesRejectsBadIDs(t *testing.T) {
	e := setup(t)
	if code := e.get(t, "/api/conversations/abc/messages", nil); code != 400 {
		t.Fatalf("non-numeric: %d", code)
	}
	if code := e.get(t, "/api/conversations/"+itoa(e.alice.ID)+"/messages", nil); code != 400 {
		t.Fatalf("self: %d", code)
	}
}

func TestRequiresSession(t *testing.T) {
	e := setup(t)
	resp, _ := http.Get(e.srv.URL + "/api/users")
	if resp.StatusCode != 401 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func itoa(i int64) string { return strconv.FormatInt(i, 10) }

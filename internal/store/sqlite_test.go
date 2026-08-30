package store

import (
	"context"
	"errors"
	"testing"
)

func open(t *testing.T) *SQLite {
	t.Helper()
	s, err := OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestUpsertUserIsIdempotentAndCaseInsensitive(t *testing.T) {
	s, ctx := open(t), context.Background()
	a, err := s.UpsertUser(ctx, "Alice")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.UpsertUser(ctx, " alice ")
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID {
		t.Fatalf("expected same user, got %d and %d", a.ID, b.ID)
	}
	if _, err := s.UpsertUser(ctx, "  "); err == nil {
		t.Fatal("empty username should fail")
	}
	users, _ := s.ListUsers(ctx)
	if len(users) != 1 {
		t.Fatalf("want 1 user, got %d", len(users))
	}
}

func TestAppendAndListMessages(t *testing.T) {
	s, ctx := open(t), context.Background()
	alice, _ := s.UpsertUser(ctx, "alice")
	bob, _ := s.UpsertUser(ctx, "bob")
	carol, _ := s.UpsertUser(ctx, "carol")

	m1, dup, err := s.AppendMessage(ctx, alice.ID, bob.ID, "hi bob", "c1")
	if err != nil || dup {
		t.Fatalf("m1: %v dup=%v", err, dup)
	}
	m2, _, _ := s.AppendMessage(ctx, bob.ID, alice.ID, "hi alice", "c2")
	_, _, _ = s.AppendMessage(ctx, alice.ID, carol.ID, "hi carol", "c3")

	// Both directions land in the same conversation regardless of argument order.
	if m1.ConversationID != m2.ConversationID {
		t.Fatal("a<->b messages should share one conversation")
	}
	if m1.RecipientID != bob.ID || m2.RecipientID != alice.ID {
		t.Fatal("recipient not derived correctly")
	}

	msgs, err := s.ListMessages(ctx, bob.ID, alice.ID, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].ID != m1.ID || msgs[1].ID != m2.ID {
		t.Fatalf("unexpected history: %+v", msgs)
	}
	// Cursor pagination.
	msgs, _ = s.ListMessages(ctx, alice.ID, bob.ID, m1.ID, 50)
	if len(msgs) != 1 || msgs[0].ID != m2.ID {
		t.Fatalf("cursor failed: %+v", msgs)
	}
	// Carol's conversation is isolated.
	msgs, _ = s.ListMessages(ctx, bob.ID, carol.ID, 0, 50)
	if len(msgs) != 0 {
		t.Fatalf("bob<->carol should be empty, got %d", len(msgs))
	}
}

func TestAppendMessageDedupesClientMsgID(t *testing.T) {
	s, ctx := open(t), context.Background()
	alice, _ := s.UpsertUser(ctx, "alice")
	bob, _ := s.UpsertUser(ctx, "bob")

	first, _, err := s.AppendMessage(ctx, alice.ID, bob.ID, "once", "same-key")
	if err != nil {
		t.Fatal(err)
	}
	// A retry with a different body but the same key must return the original.
	again, dup, err := s.AppendMessage(ctx, alice.ID, bob.ID, "retry", "same-key")
	if err != nil {
		t.Fatal(err)
	}
	if !dup || again.ID != first.ID || again.Body != "once" {
		t.Fatalf("dedupe failed: dup=%v %+v", dup, again)
	}
	// The same key from a *different* sender is a different message.
	_, dup, _ = s.AppendMessage(ctx, bob.ID, alice.ID, "mine", "same-key")
	if dup {
		t.Fatal("keys are scoped per sender")
	}
	// Reusing the key towards a different recipient is refused.
	carol, _ := s.UpsertUser(ctx, "carol")
	if _, _, err := s.AppendMessage(ctx, alice.ID, carol.ID, "again", "same-key"); !errors.Is(err, ErrClientMsgIDReused) {
		t.Fatalf("want ErrClientMsgIDReused, got %v", err)
	}
	if _, _, err := s.AppendMessage(ctx, alice.ID, alice.ID, "self", "x"); err == nil {
		t.Fatal("self-message should fail")
	}
}

func TestGetUserNotFound(t *testing.T) {
	s := open(t)
	if _, err := s.GetUser(context.Background(), 999); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

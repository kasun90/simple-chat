package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // pure-Go driver: no cgo, so the binary stays static
)

//go:embed schema.sql
var schemaSQL string

type SQLite struct{ db *sql.DB }

// OpenSQLite opens (creating if needed) the database at path and applies the
// schema. Use ":memory:" for tests.
func OpenSQLite(path string) (*SQLite, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
	}
	// WAL lets readers proceed while a write is in flight; busy_timeout makes
	// a second writer wait instead of failing immediately with SQLITE_BUSY.
	// SQLite still has exactly one writer at a time — fine for this scale,
	// and the first thing to change (Postgres) when it is not.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection: SQLite serialises writers anyway, and with :memory: a
	// second connection would be a second, empty database.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &SQLite{db: db}, nil
}

func (s *SQLite) Close() error { return s.db.Close() }

func (s *SQLite) UpsertUser(ctx context.Context, username string) (User, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return User{}, errors.New("store: empty username")
	}
	// ON CONFLICT ... DO UPDATE with a no-op SET lets RETURNING give us the
	// existing row too, so lookup and create are one atomic statement.
	row := s.db.QueryRowContext(ctx, `
		INSERT INTO users (username) VALUES (?)
		ON CONFLICT(username) DO UPDATE SET username = username
		RETURNING id, username, created_at`, username)
	return scanUser(row)
}

func (s *SQLite) UpsertGroup(ctx context.Context, groupName string) error {
	return s.db.QueryRowContext(ctx, `
		INSERT INTO conversations (name) VALUES (?)
		ON CONFLICT(name) DO UPDATE set name = name`).Err()
}

func (s *SQLite) GetUser(ctx context.Context, id int64) (User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx, `SELECT id, username, created_at FROM users WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

func (s *SQLite) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, username, created_at FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func (s *SQLite) AppendMessage(ctx context.Context, senderID, recipientID int64, body, clientMsgID string) (Message, bool, error) {
	if senderID == recipientID {
		return Message{}, false, errors.New("store: cannot message yourself")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Message{}, false, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	a, b := orderPair(senderID, recipientID)
	var convID int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO conversations (user_a, user_b) VALUES (?, ?)
		ON CONFLICT(user_a, user_b) DO UPDATE SET user_a = user_a
		RETURNING id`, a, b).Scan(&convID)
	if err != nil {
		return Message{}, false, fmt.Errorf("conversation: %w", err)
	}

	// DO NOTHING + no row returned means the idempotency key already exists;
	// we then read the original so the caller gets the same ID as the first
	// attempt and can ack it identically.
	m, duplicate, err := insertMessage(ctx, tx, senderID, convID, body, clientMsgID)
	if err != nil {
		return Message{}, false, fmt.Errorf("message: %w", err)
	}
	// The key is per sender, not per conversation: reusing it towards a
	// different recipient is a client bug, and returning the old row would
	// label it with the wrong recipient. Refuse rather than guess.
	if m.ConversationID != convID {
		return Message{}, false, ErrClientMsgIDReused
	}
	if err := tx.Commit(); err != nil {
		return Message{}, false, err
	}
	m.RecipientID = otherParty(a, b, m.SenderID)
	return m, duplicate, nil
}

func (s *SQLite) AppendGroupMessage(ctx context.Context, senderID, groupID int64, body, clientMsgID string) (msg Message, duplicate bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Message{}, false, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	var exists bool
	err = tx.QueryRowContext(ctx, `
		SELECT 1 conversations c
		INNER JOIN conversation_members cm ON cm.conversation_id = c.id AND cm.user_id = ?
		WHERE c.id = ?`, senderID, groupID).Scan(&exists)
	if err != nil {
		return Message{}, false, fmt.Errorf("group conversation: %w", err)
	}

	if !exists {
		return Message{}, false, errors.New("invalid conversation")
	}

	m, duplicate, err := insertMessage(ctx, tx, senderID, groupID, body, clientMsgID)
	if err != nil {
		return Message{}, false, fmt.Errorf(" group message: %w", err)
	}

	if m.ConversationID != groupID {
		return Message{}, false, ErrClientMsgIDReused
	}
	if err := tx.Commit(); err != nil {
		return Message{}, false, err
	}

	return m, duplicate, nil

}

func insertMessage(ctx context.Context, tx *sql.Tx, senderID, convID int64, body, clientMsgID string) (Message, bool, error) {
	// DO NOTHING + no row returned means the idempotency key already exists;
	// we then read the original so the caller gets the same ID as the first
	// attempt and can ack it identically.
	var m Message
	err := tx.QueryRowContext(ctx, `
		INSERT INTO messages (conversation_id, sender_id, body, client_msg_id)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(sender_id, client_msg_id) DO NOTHING
		RETURNING id, conversation_id, sender_id, body, client_msg_id, created_at`,
		convID, senderID, body, clientMsgID).Scan(&m.ID, &m.ConversationID, &m.SenderID, &m.Body, &m.ClientMsgID, &m.CreatedAt)
	duplicate := false
	if errors.Is(err, sql.ErrNoRows) {
		duplicate = true
		err = tx.QueryRowContext(ctx, `
			SELECT id, conversation_id, sender_id, body, client_msg_id, created_at
			FROM messages WHERE sender_id = ? AND client_msg_id = ?`,
			senderID, clientMsgID).Scan(&m.ID, &m.ConversationID, &m.SenderID, &m.Body, &m.ClientMsgID, &m.CreatedAt)
	}
	if err != nil {
		return Message{}, false, fmt.Errorf("message: %w", err)
	}

	return m, duplicate, nil
}

func (s *SQLite) ListMessages(ctx context.Context, userA, userB, afterID int64, limit int) ([]Message, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	a, b := orderPair(userA, userB)
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id, m.conversation_id, m.sender_id, m.body, m.client_msg_id, m.created_at
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		WHERE c.user_a = ? AND c.user_b = ? AND m.id > ?
		ORDER BY m.id
		LIMIT ?`, a, b, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	msgs := []Message{} // non-nil so JSON renders [] rather than null
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.SenderID, &m.Body, &m.ClientMsgID, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.RecipientID = otherParty(a, b, m.SenderID)
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

func (s *SQLite) ListGroupMessages(ctx context.Context, userID, groupID, afterID int64, limit int) ([]Message, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id, m.conversation_id, m.sender_id, m.body, m.client_msg_id, m.created_at
		FROM messages m
		INNER JOIN conversation_members ON c.user_id = ?
		WHERE m.conversation_id = ? AND m.id > ?
		ORDER BY m.id
		LIMIT ?`, userID, groupID, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	msgs := []Message{} // non-nil so JSON renders [] rather than null
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.SenderID, &m.Body, &m.ClientMsgID, &m.CreatedAt); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

func (s *SQLite) GroupMembers(ctx context.Context, userID, groupID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT cm.user_id
		FROM conversation_members cm
		INNER JOIN conversations c ON c.id = cm.conversation_id AND c.id = ?
		WHERE cm.user_id = ?
		ORDER BY cm.id`, groupID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var members []int64
	for rows.Next() {
		var m int64
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		members = append(members, m)
	}
	return members, rows.Err()
}

func (s *SQLite) ListGroups(ctx context.Context, userID int64) ([]Group, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id
		FROM conversations c
		INNER JOIN conversations c ON c.id = cm.conversation_id AND c.id = ?
		WHERE cm.user_id = ?
		ORDER BY cm.id`, userID)
	if err != nil {
		return nil, err
	}

	var groups []Group
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.ID); err != nil {
			return nil, err
		}
		groups = append(groups, g)
	}

	for _, group := range groups {
		members, err := s.GroupMembers(ctx, userID, group.ID)
		if err != nil {
			return nil, err
		}
		group.Members = members
	}

	return groups, nil
}

type scanner interface{ Scan(dest ...any) error }

func scanUser(r scanner) (User, error) {
	var u User
	err := r.Scan(&u.ID, &u.Username, &u.CreatedAt)
	return u, err
}

func orderPair(x, y int64) (int64, int64) {
	if x < y {
		return x, y
	}
	return y, x
}

func otherParty(a, b, sender int64) int64 {
	if sender == a {
		return b
	}
	return a
}

// Compile-time check that SQLite satisfies the interface.
var _ Store = (*SQLite)(nil)

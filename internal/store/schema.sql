-- created_at columns are declared DATETIME (not TEXT) because the pure-Go
-- driver only converts to time.Time for DATETIME/TIMESTAMP declared types.
-- Applied on every boot inside a transaction; every statement is idempotent.
-- A single schema file (no migration tool) is enough while the schema is young;
-- DECISIONS.md records when to graduate to numbered migrations.

CREATE TABLE IF NOT EXISTS users (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    username   TEXT NOT NULL UNIQUE COLLATE NOCASE,
    created_at DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

-- One row per unordered pair. user_a < user_b is enforced so (1,2) and (2,1)
-- can never become two conversations.
CREATE TABLE IF NOT EXISTS conversations (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    user_a     INTEGER REFERENCES users(id),
    user_b     INTEGER REFERENCES users(id),
    name       TEXT,
    created_at DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    CHECK ((user_a IS NULL AND user_b IS NULL) || (user_a IS NULL AND name IS NOT NULL)),
    CHECK (user_a < user_b),
    UNIQUE (user_a, user_b)
);

CREATE TABLE IF NOT EXISTS conversation_members (
    user_id         INTEGER NOT NULL REFERENCES users(id),
    conversation_id INTEGER NOT NULL REFERENCES conversations(id),
    PRIMARY KEY (user_id, conversation_id)
);

CREATE TABLE IF NOT EXISTS messages (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    conversation_id INTEGER NOT NULL REFERENCES conversations(id),
    sender_id       INTEGER NOT NULL REFERENCES users(id),
    body            TEXT NOT NULL,
    client_msg_id   TEXT NOT NULL,
    created_at      DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    -- Idempotency key: a retried send from the same sender is a duplicate.
    UNIQUE (sender_id, client_msg_id)
);

-- History reads are always "messages in conversation X after id Y".
CREATE INDEX IF NOT EXISTS messages_conversation_id_idx ON messages (conversation_id, id);

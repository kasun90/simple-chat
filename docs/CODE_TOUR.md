# Code tour

A reading order through the codebase, what to notice in each file, and where the likely
follow-up changes would go. Total code is small on purpose: about 1,300 lines of Go
(excluding tests) and 300 lines of JavaScript.

## Reading order

### 1. `cmd/server/main.go` — the process
Reads env config, opens the store, calls `app.New`, runs the HTTP server, shuts down
gracefully on SIGTERM. Notice: no global read/write timeouts on `http.Server` because they
would kill long-lived WebSockets; `-healthcheck` exists because distroless has no curl.

### 2. `internal/app/app.go` — the wiring
One function assembles every handler. Read this to see the whole route table:

| Route | Handler | Auth |
|---|---|---|
| `GET /healthz` | inline | no |
| `POST /api/login`, `POST /api/logout` | `auth.Handlers` | no |
| `GET /api/me` | `auth.Handlers` | cookie |
| `GET /api/users` | `api.Handlers` | cookie |
| `GET /api/conversations/{userID}/messages` | `api.Handlers` | cookie |
| `GET /ws` | `hub.Handler` | cookie + Origin |
| `/` | `web.Handler` (embedded files) | no |

### 3. `internal/ws/frame.go` then `conn.go` — the transport
`frame.go` is the RFC 6455 byte format: `readFrame` (with the allocation guard) and
`writeFrame`. `conn.go` is the handshake (`Upgrade`, `AcceptKey`) and the connection
loop (`ReadText` handles ping/pong/close internally so callers only see text). Notice the
mutex in `write`: two goroutines writing to one TCP stream would interleave bytes.

### 4. `internal/store/store.go`, `schema.sql`, `sqlite.go` — persistence
`store.go` is the interface and the two domain structs. `schema.sql` has the two
invariants that matter: `user_a < user_b` (one conversation per pair) and
`UNIQUE(sender_id, client_msg_id)` (idempotent retries). `sqlite.go` does everything with
`INSERT … ON CONFLICT … RETURNING` so lookup-or-create is one statement.

### 5. `internal/hub/hub.go` then `handler.go` — the routing
`Serve` is the per-connection loop. `handleInbound` is validate → persist → fan out.
`broadcast` / `pushRaw` are where the "never block the hub" rule lives. `register` sends
the presence snapshot under the lock so no presence change can slip between snapshot and
subscription. `handler.go` is the upgrade behind `auth.SameOrigin` (the same check that guards login).

### 6. `internal/auth/session.go`, `handlers.go` — identity
In-memory token map with TTL, cookie attributes with the reasoning for each, middleware
that puts the user ID into the request context, and `SameOrigin` — the CSRF check shared by
login, logout and the WebSocket upgrade. `handlers.go` has the strict username whitelist.

### 7. `internal/api/api.go` — REST
Contacts with presence via the `Presence` interface; history with cursor. `statusWriter`
must re-expose `Hijack` or the WebSocket upgrade would break behind the logging middleware
— a classic Go gotcha.

### 8. `web/app.js` — the client
Five sections: state, api, socket, render, events. Everything renders from `state`.
`addMessage` is the merge point for live events and history (dedupe by ID, sort by ID,
replace optimistic bubble by `clientMsgId`). `connect` has the backoff.

### 9. Tests
| File | Proves |
|---|---|
| `ws/ws_test.go` | RFC vector, all length forms, hostile length rejected before alloc, real handshake via httptest |
| `store/sqlite_test.go` | conversation symmetry, cursor, per-sender dedupe |
| `hub/hub_test.go` | multi-tab delivery, duplicate handling, validation matrix, presence, slow consumer disconnected |
| `auth/auth_test.go` | login/me/logout, validation, expiry |
| `api/api_test.go` | contacts, cursor, isolation, 401 |
| `e2e/e2e_test.go` | the whole stack through a real socket: send, retry, offline resync, origin check |

Run everything: `make check`. The Dockerfile runs vet + tests during the image build too.

## Likely change requests and where they go

| Request | Where | Sketch |
|---|---|---|
| **Typing indicator** | `hub.go` `handleInbound` + `app.js` | new inbound `{type:"typing", to}`; hub forwards `{type:"typing", userId}` to recipient's sockets **without** persisting; client shows "typing…" and clears it after 3 s. No DB change. |
| **Read receipts** | `schema.sql`, `store`, `hub`, `app.js` | `conversation_reads(conversation_id, user_id, last_read_id)`; inbound `{type:"read", to, upTo}`; hub upserts and forwards; client renders ticks for `id <= otherLastRead`. |
| **Message edit / delete** | `schema.sql`, `store`, `hub`, `app.js` | add `edited_at`, `deleted_at` columns; new inbound types; fan out `{type:"message_updated"}`; client patches by `id`. Only the sender may edit — check `sender_id` in the store. |
| **Group chat** | `schema.sql`, `store`, `hub` | `conversations` gets a `kind`; new `conversation_members` table; `AppendMessage` takes a conversation ID; fan-out iterates members. The biggest change — say so. |
| **Postgres** | new `internal/store/postgres.go` + compose service | implement `Store`; `RETURNING` and `ON CONFLICT` are already Postgres syntax; swap `AUTOINCREMENT` for `BIGSERIAL`, `DATETIME` for `TIMESTAMPTZ`. |
| **Search messages** | `store`, `api` | SQLite FTS5 virtual table or `LIKE` for a start; `GET /api/search?q=`. |
| **Unread counts server-side** | `store`, `api` | needs read receipts first; then `COUNT(*) WHERE id > last_read_id` per conversation. |
| **Rate limiting** | `hub.go` `Serve` | token bucket per client (`time.Ticker` or `x/time/rate`); reject with `{type:"error"}`. |
| **Message size limit change** | `hub.go` `MaxBodyRunes`, `index.html` `maxlength` | two constants. |
| **Username rules** | `auth/handlers.go` `usernameRe`, `index.html` `maxlength` | one regexp. |
| **Different ping interval** | `hub.go` `KeepAliveInterval` | one constant; keep `ReadTimeout` at 2×. |
| **Allowed origins list** | `auth/session.go` `SameOrigin` | compare against a slice from config instead of `r.Host`. |
| **Persist sessions** | `auth/session.go` | replace the map with a `sessions` table; same three methods. |

## Things to be ready to explain

- Why `Hijacker` and a hand-written 101 response (the http.Server no longer owns the conn).
- Why client frames must be masked (proxy cache poisoning, RFC 6455 §10.3) and server
  frames must not be.
- Why one writer goroutine per socket and a non-blocking channel send.
- Why persist-before-fanout, and what it costs.
- Why the cursor is an ID and not a timestamp.
- What changes for two replicas (OBSTACLES §7) — the most likely "scale it" question.

# Decisions

Short architecture decision records. Each says what was chosen, why, and what would make
us revisit it. The point is not that every choice is final — it is that each was a choice.

## 1. Go standard library, no web framework
**Chosen:** `net/http` with the 1.22+ pattern router; no Gin/Echo/Fiber.
**Why:** The brief rewards building one level below the integration layer. `net/http`
already gives routing, middleware (plain function composition) and `Hijacker`, which the
WebSocket layer needs. A framework would add a dependency and hide the exact thing being
demonstrated.
**Revisit when:** the route table needs versioning, OpenAPI generation, or per-route
middleware chains long enough that hand composition gets noisy.

## 2. Hand-written WebSocket instead of gorilla/websocket or nhooyr
**Chosen:** ~250 lines implementing RFC 6455 server side: handshake, framing, masking,
control frames, close handshake.
**Why:** It *is* the core feature of a real-time chat, and the brief forbids importing the
core. Scope was cut to exactly what a JSON chat needs (text frames only) so the surface
area stays small enough to own.
**Revisit when:** we need binary payloads (file transfer), compression
(`permessage-deflate`), or fragmentation for very large messages. At that point a
battle-tested library is the right call and the `Socket` interface in `hub` makes the swap
local.

## 3. SQLite (pure Go) over Postgres
**Chosen:** `modernc.org/sqlite`, WAL mode, one file on a volume.
**Why:** One container, zero infrastructure, no cgo (static binary on distroless). The
Store interface means the hub/API never know which database is underneath.
**Revisit when:** (a) more than one server replica — SQLite cannot be shared across
hosts; (b) write throughput matters — SQLite has one writer at a time; (c) we need
online schema migrations. All three arrive together with "run two replicas", which is the
first production scaling step. Implementation cost: one new file implementing `Store`,
plus a compose service.

## 4. Vanilla HTML/JS over React/Vue
**Chosen:** three static files, no bundler, served from the binary via `embed.FS`.
**Why:** The follow-up interview requires live edits without tooling. The UI is two panes
and a form; a component framework would add a toolchain, a second Dockerfile stage and a
node_modules directory to a project whose interesting part is the backend.
**Revisit when:** the UI grows real state complexity (message editing, threads, media
previews) or needs a design system shared with other apps.

## 5. Username-only login with server-side session cookie
**Chosen:** `POST /api/login {username}` upserts a user and sets an `HttpOnly`,
`SameSite=Lax` cookie holding an opaque random token; sessions live in process memory.
**Why:** The demo needs two identities, not security. The cookie plumbing is the part that
survives when passwords are added, and using a cookie (not a token in JS) means the
WebSocket upgrade is authenticated for free — browsers send cookies on the upgrade.
**Revisit when:** real users. Add password hashing (argon2id), move sessions to the DB or
Redis so a restart / second replica keeps people logged in, set `Secure` behind TLS.

## 6. Database autoincrement ID as the message order, not timestamps or UUIDs
**Chosen:** `messages.id INTEGER PRIMARY KEY AUTOINCREMENT` is the cursor and the sort key.
**Why:** Client clocks are not comparable and server clocks can step backwards (NTP).
An ID assigned inside the insert transaction is monotonic per table and free. UUIDs would
need a separate sequence column to paginate.
**Revisit when:** messages are written by more than one database node (sharding). Then a
per-conversation sequence or a ULID/Snowflake scheme with an explicit tie-break is needed,
and the client cursor logic stays the same because it only relies on "comparable and
increasing".

## 7. Client-generated idempotency key (`clientMsgId`)
**Chosen:** the browser makes a UUID per send; `UNIQUE(sender_id, client_msg_id)` in the
DB; a retry returns the original row.
**Why:** The socket can drop between "server stored it" and "client saw the echo". Without
a key, the natural client reaction (resend) duplicates the message. Scoping the key per
sender means clients cannot collide with each other and cannot suppress someone else's
message by guessing a key.
**Revisit when:** never, really — but the key table should be pruned or time-bucketed if
the messages table is ever partitioned.

## 8. Persist before fan-out
**Chosen:** `AppendMessage` commits, then the hub pushes.
**Why:** If we pushed first and crashed before commit, the recipient would have seen a
message that is not in history and can never be re-fetched — the one inconsistency a chat
user actually notices. The cost is a few milliseconds of latency per message.
**Revisit when:** latency requirements are sub-10ms at high write volume; then a
write-ahead queue (persist to log, fan out, apply to DB asynchronously) is the pattern,
with the same "durable before visible" invariant.

## 9. Non-blocking push with slow-consumer disconnect
**Chosen:** each socket has a 64-message buffer; if it is full the hub closes that socket
rather than waiting.
**Why:** Blocking would let one stalled browser tab hold the hub lock and freeze every
user. Disconnecting is safe because the client reconnects and resyncs by cursor — nothing
is lost, just delayed.
**Revisit when:** clients on very slow links legitimately fall behind during bursts; then
raise the buffer or coalesce events, but keep the "never block the hub" rule.

## 10. Single schema file applied on boot, no migration tool
**Chosen:** `schema.sql` with `CREATE ... IF NOT EXISTS`, executed at startup.
**Why:** Three tables, one developer, no deployed data to migrate.
**Revisit when:** the first `ALTER TABLE` on a database that already holds real messages.
Move to numbered migrations (goose or plain SQL files with a `schema_version` table) at that
moment, not before.

## 11. Tests run inside the Docker build
**Chosen:** the Dockerfile's build stage runs `go vet` and `go test` before `go build`.
**Why:** The deliverable is "runs in Docker". Putting the gate inside the image build makes
it impossible to publish an image whose tests fail, regardless of who builds it or whether
CI ran.
**Revisit when:** the test suite becomes slow enough to hurt image build time; then run
tests in CI only and have CI be the only thing allowed to push images.

# Obstacles and edge cases

What breaks, *why* it breaks (the mechanism), how the MVP handles it today, and what the
fix looks like when the app grows. Ordered roughly by when each one would bite.

> Interview note: the assessors want experience, not textbook answers. Each entry below
> names the mechanism; pair it with a concrete story from your own projects when you talk
> through it.

## 1. Two browser tabs for the same user
**Mechanism:** a naive `map[userID]conn` overwrites the first tab's socket when the second
opens; the first tab silently stops receiving.
**Today:** the registry is `map[userID]set[conn]`; every event goes to every socket of the
user, including the sender's other tabs (so a message typed in tab 1 appears in tab 2).
**Later:** nothing structural; per-device read state if "read receipts" arrive.

## 2. Retry after a lost ack duplicates the message
**Mechanism:** the socket dies after the server committed but before the echo reached the
client. The client's only sane move is to resend, which stores a second copy.
**Today:** `clientMsgId` + `UNIQUE(sender_id, client_msg_id)`. The retry returns the
original row and is echoed only to the sender, so the recipient never sees a duplicate.
**Later:** the pending state in the UI should actually retry on reconnect (today it stays
"pending" until the page is reloaded; the server side is ready for it).

## 3. Messages missed while disconnected
**Mechanism:** the hub pushes only to *currently* open sockets. A user whose wifi blipped
for 3 seconds misses whatever was sent in those 3 seconds.
**Today:** the client tracks the highest message ID per conversation and asks for
`?after=<lastId>` on every reconnect. IDs are server-assigned and monotonic, so the cursor
is exact. Live events and history results are merged and deduped by ID.
**Later:** with many open conversations, one request per conversation on reconnect is
chatty; add a single "everything after global ID N for me" endpoint.

## 4. Ordering across a reconnect
**Mechanism:** a live event for message 45 can arrive *before* the history fetch that
returns 43 and 44 finishes. Rendering in arrival order shows 45, 43, 44.
**Today:** the client sorts by server ID after every insert; the ID is the only order
anyone trusts. Timestamps are display-only.
**Later:** same rule; a per-conversation sequence number if the messages table is ever
sharded (see DECISIONS §6).

## 5. One stalled client freezes everyone
**Mechanism:** if fan-out does a blocking write to each socket while holding the hub lock,
a tab that stopped reading (backgrounded, suspended laptop, TCP window full) blocks the
loop, and no other user gets any message.
**Today:** each socket has its own writer goroutine and a bounded channel; the hub does a
non-blocking send. A full buffer closes that one socket, and the client resyncs on
reconnect.
**Later:** per-user rate limiting on the inbound side too — a client can currently send as
fast as it likes.

## 6. Half-open connections
**Mechanism:** a client that vanishes without a TCP FIN (lid closed, mobile network
switch) leaves a socket that never errors. The hub keeps it forever, presence says
"online", and memory leaks one goroutine per ghost.
**Today:** server pings every 30 s; the read deadline is 60 s; a socket that produces no
frame (pong or otherwise) in that window is closed.
**Later:** tune intervals for mobile (battery vs. detection latency); expose connection
counts as a metric so leaks are visible.

## 7. Running two replicas
**Mechanism:** the hub is in-process memory. With two replicas behind a load balancer,
Alice's socket is on replica 1 and Bob's on replica 2; replica 1 stores the message and
fans out to… nobody. Sessions have the same problem: a cookie issued by replica 1 is
unknown to replica 2.
**Today:** single replica, documented.
**Later:** this is the first real scaling step and it touches three things at once:
1. Sessions → DB or Redis.
2. Fan-out → publish `message` events to a broker (Redis pub/sub, NATS) keyed by
   recipient user ID; every replica subscribes and delivers to its local sockets.
3. SQLite → Postgres (SQLite cannot be shared across hosts).
Ordering caveat: once events cross a broker, two replicas can publish for the same
conversation in an order that differs from commit order. The client-side sort-by-ID
already tolerates that; the server must not assume broker order equals DB order.

## 8. SQLite single writer
**Mechanism:** SQLite serialises writers. Under a write burst the second writer waits
(`busy_timeout`) and eventually fails with `SQLITE_BUSY`.
**Today:** WAL mode so reads never wait for writes; `busy_timeout=5000`; one connection in
the pool so contention is handled in-process rather than by the file lock.
**Later:** Postgres, when replicas arrive anyway (see §7). Chat write volume is bursty but
low per conversation, so this bites later than people expect.

## 9. Cross-site WebSocket hijacking
**Mechanism:** browsers attach cookies to WebSocket upgrade requests, and `SameSite` does
not cover them. A page on evil.example can open `ws://our-host/ws` and the browser sends
the victim's session cookie.
**Today:** the upgrade handler rejects any request whose `Origin` host differs from
`Host`. `Origin` is set by the browser and cannot be forged by page script.
**Later:** an explicit allow-list of origins when the frontend is served from a different
host than the API.

## 10. Hostile frame lengths
**Mechanism:** the WebSocket length field is client-controlled. A frame claiming 2^40
bytes makes a naive `make([]byte, n)` allocate a terabyte and crash the process.
**Today:** the length is compared to a 64 KiB cap *before* allocation; oversized frames
close the connection with 1009.
**Later:** the cap is a constant; make it configurable per deployment if larger payloads
are ever needed.

## 11. Message body limits and encoding
**Mechanism:** limiting by bytes penalises non-Latin scripts (one CJK character is 3
bytes). Not limiting at all lets one user paste a novel into everyone's database.
**Today:** 2000 *runes* server-side, `maxlength=2000` in the input.
**Later:** consistent counting in the UI for grapheme clusters (emoji sequences count as
several runes).

## 12. XSS through message bodies
**Mechanism:** rendering `innerHTML = message.body` lets a user send `<img onerror=…>`
to everyone they chat with.
**Today:** every user-controlled string is set with `textContent`. There is no markdown or
link rendering.
**Later:** if rich text is added, sanitise on render with an allow-list, never on store
(the stored body should stay what the user sent).

## 13. Contact list is the whole users table
**Mechanism:** `GET /api/users` returns everyone. Fine for a demo; a 100k-user directory
would ship megabytes to every client on every reconnect.
**Today:** unpaginated, documented.
**Later:** search-as-you-type endpoint plus a "recent conversations" list built from the
messages table (`MAX(id) GROUP BY conversation_id`).

## 14. Presence is per-process and lossy
**Mechanism:** presence is derived from open sockets in this process's memory. A client
that reconnects quickly generates an offline/online pair; a crashed server forgets
everything.
**Today:** first-connect/last-disconnect only; a snapshot on join so late joiners see the
current state.
**Later:** presence in Redis with TTL refreshed by heartbeat, which also makes it correct
across replicas.

## 15. In-memory sessions log everyone out on restart
**Mechanism:** every deploy or crash empties the session map.
**Today:** acceptable for a demo; the browser lands back on the login screen.
**Later:** persist sessions (DB table or Redis); one query per request is cheap.

## 16. No passwords, no rate limiting
**Mechanism:** anyone who knows a username can be that user; anyone can POST /api/login
a million times, creating a million users and a million 24-hour sessions. Because the
contact list is the whole users table (§13), that also turns every legitimate client's
refresh into a huge download — one attacker degrades everyone.
**Today:** deliberately out of scope for a local demo. Usernames are restricted to
`[A-Za-z0-9_.-]{1,32}` so look-alike names (zero-width characters, Cyrillic "а") cannot
impersonate someone in the contact list.
**Later:** argon2id password hashes, login rate limiting per IP and per account, a session
sweeper (expired sessions are currently only evicted on lookup), a cap on sockets per user,
`Secure` cookie behind TLS.

## 16a. Login CSRF
**Mechanism:** `SameSite` cookies protect requests that *carry* a cookie; login carries
none. A cross-site `<form enctype="text/plain">` can POST a body that a lenient JSON
decoder accepts, silently logging the victim in as an attacker-owned account — after
which everything they type is readable by the attacker.
**Today:** login and logout require a same-origin `Origin` header (when present) and login
requires `Content-Type: application/json`, which HTML forms cannot produce.
**Later:** an explicit origin allow-list if the frontend moves to another host.

## 17. Data lifecycle
**Mechanism:** messages are never deleted. Storage grows forever and there is no way to
honour a "delete my data" request.
**Today:** append-only.
**Later:** soft-delete flag + retention job; export endpoint; `ON DELETE CASCADE` from
users is *not* the answer for the other party's history — tombstone instead.

## 18. Schema changes on live data
**Mechanism:** `CREATE TABLE IF NOT EXISTS` cannot express "add a column".
**Today:** one schema file, no migrations, no deployed data.
**Later:** numbered migration files the moment the first real `ALTER TABLE` is needed.

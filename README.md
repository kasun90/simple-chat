# simple-chat

A real-time 1-to-1 messaging web app (think a minimal WhatsApp Web), built for the GovTech
Senior Full Stack take-home assessment.

The core is implemented from primitives: the WebSocket layer is hand-written on top of Go's
`net/http` (RFC 6455), and the frontend is plain HTML/JS with no build step. See `docs/`.

## Quick start

```bash
docker compose up --build        # builds, runs tests inside the build, starts on :8080
```

Open http://localhost:8080 in two browser windows, log in as `alice` and `bob`, and chat.

Or without cloning (image published by CI):

```bash
docker run --rm -p 8080:8080 -e SEED_USERS=alice,bob ghcr.io/kasun90/simple-chat
```

## Demo script (≈30 s)

1. Window 1 → http://localhost:8080 → log in as `alice`. Window 2 → log in as `bob`.
2. Alice clicks **bob**, sends "hi" → appears instantly in Bob's window with an unread badge.
3. Bob replies → appears instantly for Alice. Presence dots are green while both are open.
4. Refresh Alice's window → still logged in, history is there.
5. Close Bob's window → his dot turns grey for Alice. Reopen → he catches up on anything sent meanwhile.

## What's inside

| | |
|---|---|
| Backend | Go 1.25, standard library only for HTTP and WebSocket |
| WebSocket | hand-written RFC 6455 server (`internal/ws`) — handshake, framing, masking, ping/pong, close |
| Storage | SQLite via a pure-Go driver, behind a `Store` interface |
| Frontend | vanilla HTML/JS/CSS, embedded in the binary |
| Container | multi-stage build, tests run inside the build, distroless non-root runtime, ~15 MB |
| CI | gofmt, vet, race tests, Docker build; image pushed to GHCR on `main` |

## Development

```bash
make run     # go run with seeded users
make check   # vet + race tests + build — the same gate CI enforces
```

## Docs

- `docs/ARCHITECTURE.md` — how it works, with diagrams
- `docs/DECISIONS.md` — why each choice was made and when to revisit it
- `docs/OBSTACLES.md` — known edge cases and what breaks at scale
- `docs/CODE_TOUR.md` — a guided reading order through the code
- `docs/DEMO.md` — demo script and video checklist

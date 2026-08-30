# Demo and video checklist

## Before recording

```bash
docker compose down -v          # clean slate: empty database
make demo                       # builds, starts, waits for healthy, opens two windows
```

Arrange the two browser windows side by side. Use a normal window and a private/incognito
window, or two different browsers — a session cookie is per browser profile, so two tabs
of the same profile would be the *same* user.

## 30-second script

| t | Action | What the viewer sees |
|---|---|---|
| 0 s | Left: log in as `alice`. Right: log in as `bob`. | Both land on the contact list; each sees the other with a green dot. |
| 6 s | Alice clicks **bob**, types "hi bob 👋", Enter. | Bubble appears on the left; on the right an unread badge **1** appears on *alice* instantly. |
| 12 s | Bob clicks **alice**, replies "hey alice". | Both messages in both windows, in order. |
| 18 s | Refresh Alice's window (⌘R). | Still logged in, history reloads. |
| 23 s | Close Bob's window. | Alice sees Bob's dot go grey. |
| 27 s | Alice sends "are you there?"; reopen Bob's window, log in as bob, open alice. | Bob's history shows the message sent while he was gone. |

Optionally show `docker compose ps` for a second at the start: one container.

## Recording on macOS

QuickTime Player → File → New Screen Recording → select the region with both windows.
Trim to ≤ 30 s (⌘T). Export 1080p. Upload as an unlisted YouTube video or a Drive link.

## If something looks off

- Both windows show the *same* user → same browser profile; use incognito for one.
- Grey dot for someone who is online → presence arrives over the socket a moment after
  the contact list loads.
- Status dot tooltip says "reconnecting…" → the server restarted; the client reconnects
  with backoff within 1–10 s and re-syncs history.

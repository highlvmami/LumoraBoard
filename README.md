# LumoraBoard

Real-time collaborative whiteboard. Go + WebSocket backend built on a room-per-goroutine actor model, Svelte frontend.

## Layout

| Path | What |
|---|---|
| `backend/` | Go server (`cmd/server`, `internal/...`) |
| `frontend/` | SvelteKit + TypeScript app |
| `docs/roadmap.md` | Phased roadmap (Turkish) |

## Development

Requirements: Go 1.24+, Node 22+, Docker.

```sh
cd frontend && npm install && cd ..
make dev        # Postgres + backend on :8080 + frontend on :5173
make test       # go test -race, vitest, svelte-check
make lint       # go vet + golangci-lint
```

The frontend dev server proxies `/healthz` and `/ws` to the backend.

## Persistence

With `LUMORA_DATABASE_URL` set, rooms persist to Postgres (the schema is applied at startup). Each room has a write-behind persister: accepted ops are batched and written off the room goroutine, a snapshot every 1000 ops lets the store drop the ops it covers, and a room that restarts loads the latest snapshot plus the ops after it. If the database falls behind, the persister's bounded queue fills and the room pauses taking new ops (cursors keep flowing) until it catches up; accepted ops are never dropped. Without the variable the server runs in memory only.

Store tests against a real database run when `LUMORA_TEST_DATABASE_URL` is set; `make test` sets it to the docker-compose database.

## Sign-in and access

Boards are private once a sign-in provider is configured. The first person to open a board owns it and shares it with invite links that grant `editor` or `viewer`. Viewers see the board and cursors live, but the room rejects their ops. Sessions are server-side (only a SHA-256 of the cookie token is stored) in an `HttpOnly`, `SameSite=Lax` cookie, `Secure` when the public URL is https. OAuth uses the authorization code flow with PKCE.

| Variable | Meaning |
|---|---|
| `LUMORA_PUBLIC_URL` | Where browsers reach the app; callback URLs are `<this>/auth/{github,google}/callback`. Default `http://localhost:5173`. |
| `LUMORA_GITHUB_CLIENT_ID`, `LUMORA_GITHUB_CLIENT_SECRET` | Enable GitHub sign-in. |
| `LUMORA_GOOGLE_CLIENT_ID`, `LUMORA_GOOGLE_CLIENT_SECRET` | Enable Google sign-in. |
| `LUMORA_DEV_LOGIN=1` | Sign in with just a name. Development only; `make dev` turns it on. |
| `LUMORA_GUESTS=view` | Let people who are not signed in or not invited watch boards read-only. |

With no provider configured the server stays open: everyone can draw on every board, as before.

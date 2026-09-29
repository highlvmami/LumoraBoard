# LumoraBoard

Real-time collaborative whiteboard. Go + WebSocket backend built on a room-per-goroutine actor model, Svelte frontend.

## Layout

| Path | What |
|---|---|
| `backend/` | Go server (`cmd/server`, `internal/...`) |
| `frontend/` | SvelteKit + TypeScript app |
| `docs/roadmap.md` | Phased roadmap (Turkish) |

## Development

Requirements: Go 1.25+, Node 22+, Docker.

```sh
cd frontend && npm install && cd ..
make dev        # Postgres + backend on :8080 + frontend on :5173
make test       # go test -race, vitest, svelte-check
make lint       # go vet + golangci-lint
```

The frontend dev server proxies `/healthz` and `/ws` to the backend.

## Persistence

With `LUMORA_DATABASE_URL` set, rooms persist to Postgres (the schema is applied at startup). Each room has a write-behind persister: accepted ops are batched and written off the room goroutine, a snapshot every 1000 ops lets the store drop the ops it covers, and a room that restarts loads the latest snapshot plus the ops after it. If the database falls behind, the persister's bounded queue fills and the room pauses taking new ops (cursors keep flowing) until it catches up; accepted ops are never dropped. Without the variable boards live in process memory until the server restarts.

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

## Chat

Each board has a chat on the same socket. `chat.send` goes through the room goroutine like an op, so messages get per-board ids in one order, fan out to everyone and are written by the same write-behind persister. The latest 50 arrive with `hello`; older ones page in from `GET /api/boards/{board}/chat?before=<id>&limit=<n>`. Each account gets a token bucket (burst 5, one message a second after that) shared across its tabs; a message may point at a board object, which the panel shows as a link that selects the object and pans to it. `chat.typing` is lossy presence like cursors, at most once a second per connection. Viewers with an account may chat; anonymous guests read only.

## Export and import

PNG and SVG are drawn in the browser. High-resolution PNG (up to 4x, capped at 16M pixels), PDF and JSON backups are server jobs: `POST /api/boards/{board}/exports` puts a job on a bounded queue that a fixed pool of workers (one per core, at most four) drains. Progress goes to the requesting connection as `export.progress` messages through its room; `GET /api/exports/{id}` reports status, `GET /api/exports/{id}/file` downloads the result (kept ten minutes) and `DELETE /api/exports/{id}` cancels through the job's context. A full queue answers 503 and more than three unfinished exports per person 429, so a burst of requests waits or is turned away instead of piling up. `POST /api/boards/import` with a JSON backup validates every object like a live op and opens it as a new board owned by the importer.

## Running several servers

Set the same `LUMORA_CLUSTER_SECRET` (at least 16 characters) and `LUMORA_DATABASE_URL` on every instance and they share boards. Each board is owned by one instance at a time through a lease row in Postgres (`room_leases`); the owner renews its leases every couple of seconds. A client that lands on another instance is proxied to the owner over a WebSocket signed with the cluster secret, so the load balancer needs no sticky sessions. If the owner dies, its leases expire (6 s) and the next instance a client reaches takes the board over from the database; proxied clients get close code 1012 and reconnect. Every write carries the lease epoch, so an old owner that wakes up late cannot overwrite the new one. A graceful stop releases leases at once.

| Variable | Meaning |
|---|---|
| `LUMORA_CLUSTER_SECRET` | Turns cluster mode on; signs forwarded connections. |
| `LUMORA_ADVERTISE_URL` | How other instances reach this one. Default `http://<LUMORA_ADDR>` (127.0.0.1 if the host is empty). |
| `LUMORA_INSTANCE` | Name in the lease table and logs. Default random. |

On a crash, ops the owner accepted but had not yet flushed (normally a fraction of a second's worth) are lost, and export jobs running on it fail; clients re-sync from the database when they reconnect.

## Deploying to Fly.io

The `Dockerfile` builds one image: the Go server plus the built frontend (`LUMORA_STATIC_DIR`), served from one origin. `fly.toml` runs it as a public demo on two machines in cluster mode, with dev login on so anyone can sign in with a name.

1. Create a Postgres database on [Neon](https://neon.tech) (Frankfurt is closest to the `fra` region) and copy its connection string with connection pooling turned off.
2. Create a Fly.io account and an organization token (Dashboard → Tokens).
3. Add three repository secrets on GitHub (Settings → Secrets and variables → Actions): `FLY_API_TOKEN`, `LUMORA_DATABASE_URL` (the Neon string) and `LUMORA_CLUSTER_SECRET` (any random string of 16+ characters).
4. Run the Deploy workflow (Actions → Deploy → Run workflow). After that every green push to main deploys by itself.

The app is `lumoraboard-demo` at https://lumoraboard-demo.fly.dev; to rename it, change `app` and both URLs in `fly.toml`. With the flyctl CLI instead: `fly apps create <name>`, `fly secrets set LUMORA_DATABASE_URL=... LUMORA_CLUSTER_SECRET=...`, `fly deploy`.

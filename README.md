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

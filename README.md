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

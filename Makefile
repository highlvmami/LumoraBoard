.PHONY: dev dev-backend dev-frontend test test-backend test-frontend lint db-up db-down

dev: db-up
	$(MAKE) -j2 dev-backend dev-frontend

DATABASE_URL ?= postgres://lumora:lumora@localhost:5432/lumora

dev-backend:
	cd backend && LUMORA_DATABASE_URL=$(DATABASE_URL) LUMORA_DEV_LOGIN=1 go run ./cmd/server

dev-frontend:
	cd frontend && npm run dev

test: test-backend test-frontend

# Store tests against Postgres run when the database is up (make db-up).
test-backend:
	cd backend && LUMORA_TEST_DATABASE_URL=$(DATABASE_URL) go test -race ./...

test-frontend:
	cd frontend && npm test && npm run check

lint:
	cd backend && go vet ./... && golangci-lint run

db-up:
	docker compose up -d postgres

db-down:
	docker compose down

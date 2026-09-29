.PHONY: dev dev-backend dev-frontend test test-backend test-frontend lint db-up db-down

dev: db-up
	$(MAKE) -j2 dev-backend dev-frontend

dev-backend:
	cd backend && go run ./cmd/server

dev-frontend:
	cd frontend && npm run dev

test: test-backend test-frontend

test-backend:
	cd backend && go test -race ./...

test-frontend:
	cd frontend && npm test && npm run check

lint:
	cd backend && go vet ./... && golangci-lint run

db-up:
	docker compose up -d postgres

db-down:
	docker compose down

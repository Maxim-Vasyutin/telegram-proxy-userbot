.PHONY: build run run-dev lint fmt test clean migrate-up migrate-down

# Build a static binary into bin/userbot
build:
	CGO_ENABLED=0 go build -ldflags="-w -s" -o bin/userbot ./cmd/userbot

# Start the full stack (app + Postgres) using named volumes
run:
	docker compose -f deploy/docker-compose.yml up -d

# Start with dev overlay (bind mounts from SECRETS_DIR)
run-dev:
ifndef SECRETS_DIR
	$(error SECRETS_DIR is not set. Export it first: export SECRETS_DIR=/path/to/secrets)
endif
	docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.dev.yml up -d

# Run golangci-lint
lint:
	golangci-lint run

# Format all Go source files
fmt:
	gofmt -w .
	goimports -w .

# Run all tests
test:
	go test ./...

# Apply all pending migrations
migrate-up:
	goose -dir migrations postgres "$(DATABASE_DSN)" up

# Roll back the last migration
migrate-down:
	goose -dir migrations postgres "$(DATABASE_DSN)" down

# Remove build artifacts
clean:
	rm -rf bin/

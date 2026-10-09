.PHONY: run build test test-integration race cover generate vet db-up db-down

run:
	go run ./cmd/sitewatch

build:
	go build -o bin/sitewatch ./cmd/sitewatch

test:
	go test ./...

TEST_DATABASE_URL ?= postgres://sitewatch:sitewatch@localhost:5432/sitewatch_test?sslmode=disable

# Uses a dedicated database: the tests truncate the tables.
test-integration:
	TEST_DATABASE_URL='$(TEST_DATABASE_URL)' go test -tags integration -count=1 ./internal/postgres/

# The race detector needs cgo (on Windows: gcc from MinGW-w64 in PATH).
race:
	CGO_ENABLED=1 go test -race -count=1 ./...

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

generate:
	go generate ./...

vet:
	go vet ./...

db-up:
	docker compose up -d --wait

db-down:
	docker compose down

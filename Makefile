.PHONY: run build test test-e2e tidy fmt

run:
	go run ./cmd/server

build:
	go build -o bin/server ./cmd/server

test:
	go test ./...

# Requires Docker — spins up Postgres via testcontainers.
test-e2e:
	go test -tags e2e -count=1 ./test/e2e/...

tidy:
	go mod tidy

fmt:
	go fmt ./...

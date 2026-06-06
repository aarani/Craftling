.PHONY: run run-agent build build-agent test test-e2e tidy fmt

run:
	go run ./cmd/server

# Run a host agent (P3). Defaults target a local control plane; override via env,
# e.g. MODE=agent CONTROL_PLANE_URL=... ADVERTISE_ADDR=... PORT=9000.
run-agent:
	MODE=agent go run ./cmd/agent

build:
	go build -o bin/server ./cmd/server

build-agent:
	go build -o bin/agent ./cmd/agent

test:
	go test ./...

# Requires Docker — spins up Postgres via testcontainers.
test-e2e:
	go test -tags e2e -count=1 ./test/e2e/...

tidy:
	go mod tidy

fmt:
	go fmt ./...

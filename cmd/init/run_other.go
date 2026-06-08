//go:build !linux

package main

import "go.uber.org/zap"

// run is a no-op on dev hosts (macOS, BSDs, Windows). The init agent is
// only ever executed as PID 1 inside a Linux microVM; this stub keeps
// `go build ./...` happy from a non-Linux dev machine.
func run(logger *zap.Logger) {
	logger.Fatal("init: only supported as PID 1 inside a Linux microVM")
}

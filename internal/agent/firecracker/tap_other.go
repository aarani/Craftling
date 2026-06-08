//go:build !linux

package firecracker

import "errors"

// errTAPUnsupported is returned by the TAP helpers off Linux. The
// Firecracker driver only ever runs on a Linux/KVM host; these stubs
// exist so `go build ./...` succeeds from a non-Linux dev machine.
var errTAPUnsupported = errors.New("firecracker: TAP devices are only supported on linux")

func createTAP(string) error { return errTAPUnsupported }

func deleteTAP(string) error { return errTAPUnsupported }

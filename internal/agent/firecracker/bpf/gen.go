// Package bpf holds the compiled tapfilter eBPF program and its generated Go
// bindings. The bindings are produced by bpf2go from tapfilter.c; run
// `go generate ./...` (needs clang + libbpf headers) after editing the C.
package bpf

// Little-endian only: Firecracker hosts are x86-64/arm64, both LE.
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target bpfel -type event -type config Tapfilter tapfilter.c

// Package bpf holds the eBPF C sources for the Firecracker dataplane and the
// bpf2go-generated Go bindings + compiled objects.
//
// The generated artifacts (*_bpfel.go, *_bpfel.o) and vmlinux.h are COMMITTED,
// not built on the fly: producing them needs clang + libbpf + bpftool, which
// the deploy/CI hosts don't have, and CO-RE makes the single compiled object
// portable across kernels at load time. So `go build` needs only the Go
// toolchain. Regeneration is a maintainer step — after editing any .c file, run
// `make bpf-generate` on a Linux >= 6.6 host (it dumps vmlinux.h from the host
// BTF, then runs the directives below) and commit the changed artifacts.
//
// vmlinux.h supplies the conntrack kfunc and nf_conn struct definitions nat.c
// needs at compile time, so the generating host must have CONFIG_NF_CONNTRACK +
// CONFIG_NF_NAT in its BTF. Both programs require a kernel >= 6.6 (TCX) with
// CONFIG_DEBUG_INFO_BTF=y to load.
package bpf

// x86-64 only (little-endian), so the bpfel target matches the host.
//
// -cflags adds the multiarch include dir where libc6-dev keeps <asm/types.h>:
// clang targeting BPF does not search it on its own, so <linux/bpf.h> (pulled in
// by tapfilter.c) fails to find asm/types.h without it.

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target bpfel -cflags "-I/usr/include/x86_64-linux-gnu" -type event -type config Tapfilter tapfilter.c

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target bpfel -cflags "-I/usr/include/x86_64-linux-gnu" -type nat_event -type global_config -type vm_entry -type dnat_key -type dnat_val -type policy_key -type policy_val -type nat_stats Nat nat.c

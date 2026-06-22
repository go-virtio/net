# Performance — go-virtio virtio-net driver hot-path efficiency (2026-06-22)

This measures the **virtio-net per-frame header path** — the controllable
guest-side overhead the driver adds to every TX/RX frame: the 12-byte
`virtio_net_hdr` prepend (TX) and strip (RX), plus the MAC formatter.

The ring management (descriptor/avail/used) lives in `go-virtio/common`;
see its `BENCHMARKS.md` for the virtqueue hot-path numbers and the honest
discussion of why an end-to-end pps comparison against the **Linux kernel**
virtio-net driver is **not apples-to-apples** (end-to-end pps is set by the
host VMM's vhost backend + the physical NIC, not the guest header code).

## Methodology

- **CPU:** Apple M4 Max (16 logical CPUs). **OS:** macOS 26.5. **Go:** 1.26.4
  (`darwin/arm64`). **CGO_ENABLED=0**, `GOWORK=off`.
- **Isolated-micro:** header build/parse only — no NIC, no device, no DMA.
- Best-of-3 (`-count=3`), `-benchmem`. Values are the median.
- Reproduce:
  `GOWORK=off CGO_ENABLED=0 go test -run '^$' -bench . -benchmem -count=3 ./...`
- Benchmarks live in `bench_test.go` so they do **not** affect the 99%
  coverage gate (they run only under `-bench`).

## Results (isolated-micro — our per-frame controllable overhead)

| path | ns/op | throughput | allocs/op | note |
|------|------:|-----------:|----------:|------|
| `StripVirtioNetHdr` (RX) | 0.25 | — | 0 | length check + sub-slice, **zero-copy** |
| `MAC6.String` | 12.5 | — | 1 | diagnostics only, not the data path |
| `PrependVirtioNetHdr` 42 B (ARP) | 13.9 | 3.0 GB/s | 1 | small-frame TX; alloc dominates |
| `PrependVirtioNetHdr` 1500 B (MTU) | 170 | ~8.8 GB/s | 1 | full-MTU TX; alloc + frame copy |

## Summary

- **RX is already optimal: zero-copy, zero-alloc** (~0.25 ns). `StripVirtioNetHdr`
  just bounds-checks and re-slices past the 12-byte header — nothing to
  improve.
- **TX costs exactly one allocation per frame.** `PrependVirtioNetHdr` does
  `make([]byte, 12+len)` then copies the frame in behind the all-zero header.
  At full MTU that's ~8.8 GB/s of memcpy bandwidth — fine in isolation, but
  the **per-frame heap allocation is the one piece of controllable overhead
  worth removing**.

### Action items (our controllable overhead)

1. **Eliminate the TX per-frame allocation.** Add a
   `PrependVirtioNetHdrInto(dst, frame []byte)` (or have the TX path own a
   reusable 1518-byte scratch buffer / a small free-list) so the steady-state
   TX path is **zero-alloc**, matching the RX side. The header is always
   12 zero bytes, so this is a pure `copy` into a caller-owned buffer.
2. **Reserved-headroom frames.** If callers build frames with 12 bytes of
   leading headroom already reserved, TX becomes a zero-copy in-place header
   write (mirror of the zero-copy RX strip) — no allocation and no memcpy.

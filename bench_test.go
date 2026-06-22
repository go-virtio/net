// Driver hot-path micro-benchmarks for the virtio-net per-frame header
// build/parse path. These measure ONLY go-virtio's controllable overhead
// (the 12-byte virtio_net_hdr prepend/strip + MAC formatting) — no real
// NIC, no host VMM. End-to-end pps is dominated by the device + hypervisor;
// see BENCHMARKS.md.
//
// Run:  GOWORK=off go test -run x -bench . -benchmem ./...
//
// Benchmarks live in a _test.go file so they do NOT affect the
// statement-coverage gate (they only execute under -bench).

package net

import "testing"

// frame1500 is a full-MTU Ethernet payload — the worst-case copy for the
// TX header-prepend path.
var frame1500 = make([]byte, 1500)

// frame42 is an ARP-sized frame — the common small-frame case.
var frame42 = make([]byte, 42)

// BenchmarkPrependVirtioNetHdr1500 measures the per-frame TX overhead: one
// make([]byte, 12+len) allocation plus the copy of the Ethernet frame in
// behind the 12-byte all-zero virtio_net_hdr.
func BenchmarkPrependVirtioNetHdr1500(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(frame1500)))
	var sink []byte
	for i := 0; i < b.N; i++ {
		sink = PrependVirtioNetHdr(frame1500)
	}
	_ = sink
}

// BenchmarkPrependVirtioNetHdr42 measures the same path for a small (ARP)
// frame, where the per-call allocation dominates the byte copy.
func BenchmarkPrependVirtioNetHdr42(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(frame42)))
	var sink []byte
	for i := 0; i < b.N; i++ {
		sink = PrependVirtioNetHdr(frame42)
	}
	_ = sink
}

// BenchmarkStripVirtioNetHdr measures the per-frame RX overhead: a length
// check + a sub-slice (no copy). This is the cheap direction.
func BenchmarkStripVirtioNetHdr(b *testing.B) {
	buf := make([]byte, VirtioNetHdrSize+1500)
	b.ReportAllocs()
	b.ResetTimer()
	var sink []byte
	for i := 0; i < b.N; i++ {
		f, err := StripVirtioNetHdr(buf)
		if err != nil {
			b.Fatal(err)
		}
		sink = f
	}
	_ = sink
}

// BenchmarkMAC6String measures the dep-light MAC formatter (used in probe
// logs / diagnostics, not the data path).
func BenchmarkMAC6String(b *testing.B) {
	m := MAC6{0x52, 0x54, 0x00, 0x12, 0x34, 0x56}
	b.ReportAllocs()
	b.ResetTimer()
	var sink string
	for i := 0; i < b.N; i++ {
		sink = m.String()
	}
	_ = sink
}

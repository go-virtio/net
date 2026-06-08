// Package net is a pure-Go virtio-net (network device) driver. It
// drives a modern (Virtio 1.0+) PCI virtio-net device through the
// transport interfaces defined in github.com/go-virtio/common; the
// same code drives a UEFI-backed device, a bare-metal device, or a
// virtio-mmio device depending on which `common.Transport`
// implementation the caller supplies.
//
// Scope:
//
//   - Modern transport (VIRTIO_F_VERSION_1 mandatory). Legacy / I/O-port
//     transitional devices are NOT supported — the modern transport
//     init sequence rejects them via the underlying common package.
//   - Split-virtqueue layout. The packed-ring variant (VIRTIO_F_RING_PACKED)
//     is NOT supported; the driver negotiates it OUT.
//   - One queue pair (rxq + txq). Multi-queue (VIRTIO_NET_F_MQ) is NOT
//     negotiated.
//   - No GSO, no checksum offload (driver always emits an all-zero
//     virtio_net_hdr; on RX we ignore the device-set flags).
//
// References:
//
//   - Virtio 1.1 §5.1   "Network Device" — device-type 1 binding.
//   - Virtio 1.1 §5.1.3 "Feature bits" — VIRTIO_NET_F_*.
//   - Virtio 1.1 §5.1.4 "Device configuration layout" — MAC, status,
//                       max_virtqueue_pairs, MTU.
//   - Virtio 1.1 §5.1.6 "Device Operation" — the per-frame
//                       struct virtio_net_hdr.
//   - Virtio 1.1 §3.1.1 "Driver Requirements: Device Initialization"
//                       — the status-bit choreography in OpenVirtioNet.
//   - Linux drivers/net/virtio_net.c — canonical Go-translatable
//                       reference for the init sequence and rxq pre-post
//                       pattern.
package net

// VirtioNetHdrSize is the on-the-wire byte length of `struct
// virtio_net_hdr` (Virtio 1.1 §5.1.6.1). With VIRTIO_F_VERSION_1
// negotiated, the header is ALWAYS 12 bytes regardless of
// VIRTIO_NET_F_MRG_RXBUF — the `num_buffers` field is unconditional on
// modern devices. We negotiate VERSION_1 ON and MRG_RXBUF OFF, so 12
// bytes is what every TX/RX buffer must reserve.
//
// Field offsets (little-endian within the 12-byte block):
//
//	0      u8     flags
//	1      u8     gso_type
//	2..3   le16   hdr_len
//	4..5   le16   gso_size
//	6..7   le16   csum_start
//	8..9   le16   csum_offset
//	10..11 le16   num_buffers   (driver writes 0 on TX; device fills on RX)
const VirtioNetHdrSize = 12

// VIRTIO_NET_HDR_F_* and VIRTIO_NET_HDR_GSO_* values (Virtio 1.1
// §5.1.6.2). The driver emits all-zero headers (no GSO, no checksum
// offload).
const (
	HdrFNeedsCsum uint8 = 0x1
	HdrFDataValid uint8 = 0x2

	GSONone  uint8 = 0
	GSOTCPv4 uint8 = 1
	GSOUDP   uint8 = 3
	GSOTCPv6 uint8 = 4
	GSOECN   uint8 = 0x80
)

// VIRTIO_NET_F_* feature bits (Virtio 1.1 §5.1.3). Only the ones we
// either accept or explicitly reject are listed; the full set lives in
// the spec.
const (
	// FeatureCSUM (bit 0): driver checksum-offload support.
	// Informational; not accepted.
	FeatureCSUM uint64 = 1 << 0

	// FeatureGuestCSUM (bit 1): device may set "needs checksum" on
	// RX. Informational; not accepted.
	FeatureGuestCSUM uint64 = 1 << 1

	// FeatureMTU (bit 3): device-provided MTU.
	//
	// Required by Apple VZ — without ack'ing it, VZ clears FEATURES_OK
	// and the init aborts. Informational/no-op on QEMU+EDK2 (the bit is
	// always offered, no semantic change in the driver path).
	//
	// We don't read the device's `mtu` field; we use the default-MTU
	// 1518-byte rxq buffer per VirtioNetMaxFrameSize.
	FeatureMTU uint64 = 1 << 3

	// FeatureMAC (bit 5): device-provided MAC at DeviceCfg offset 0.
	// REQUIRED by this driver — the probe needs the device-published
	// MAC for the source field of outbound frames.
	FeatureMAC uint64 = 1 << 5

	// FeatureGuestTSO4 (bit 7), FeatureGuestTSO6 (bit 8),
	// FeatureHostTSO4 (bit 11), FeatureHostTSO6 (bit 12): TCP
	// segmentation offload. Informational; not accepted (the driver
	// emits all-zero headers).
	FeatureGuestTSO4 uint64 = 1 << 7
	FeatureGuestTSO6 uint64 = 1 << 8
	FeatureHostTSO4  uint64 = 1 << 11
	FeatureHostTSO6  uint64 = 1 << 12

	// FeatureMrgRxbuf (bit 15): merged-receive-buffer mode.
	// NOT ACCEPTED — the driver assumes one packet per buffer (no
	// chained descriptors on RX).
	FeatureMrgRxbuf uint64 = 1 << 15

	// FeatureStatus (bit 16): device publishes link-up bit in
	// DeviceCfg.status. Informational; accepted so the device
	// publishes the field.
	FeatureStatus uint64 = 1 << 16

	// FeatureMQ (bit 22): multi-queue. NOT ACCEPTED — driver uses one
	// queue pair.
	FeatureMQ uint64 = 1 << 22
)

// VirtioNetCfgOffset* — the device-specific config field offsets for
// virtio-net (Virtio 1.1 §5.1.4):
//
//	struct virtio_net_config {
//	    u8 mac[6];                 // offset 0
//	    le16 status;               // offset 6
//	    le16 max_virtqueue_pairs;  // offset 8
//	    le16 mtu;                  // offset 10
//	    // ... 1.1 additions
//	};
const (
	CfgOffsetMAC                uint32 = 0
	CfgOffsetStatus             uint32 = 6
	CfgOffsetMaxVirtqueuePairs  uint32 = 8
	CfgOffsetMTU                uint32 = 10
)

// MACLen is the byte length of the virtio-net MAC field (Virtio 1.1
// §5.1.4 — 6 bytes, IEEE 802.3 EUI-48).
const MACLen = 6

// RxQueueIdx / TxQueueIdx are the canonical queue indices for the
// single virtio-net queue pair (Virtio 1.1 §5.1.2 — "queue 0 = receive
// queue, queue 1 = transmit queue").
const (
	RxQueueIdx uint16 = 0
	TxQueueIdx uint16 = 1
)

// MaxFrameSize is the Ethernet MTU (1500) + Ethernet header (14) + 4
// bytes for VLAN headroom. We pre-post 1518-byte buffers on the rxq;
// on the txq the same size is enough for ARP (42 bytes) plus the
// virtio header.
const MaxFrameSize = 1518

// RxRingSize is the number of buffers pre-posted on the rxq. Sized for
// "a few simultaneous in-flight frames". MUST be a power of two.
const RxRingSize uint16 = 16

// TxRingSize is the txq depth. The driver issues one frame at a time
// so 8 is plenty.
const TxRingSize uint16 = 8

// VirtioNetHdr is the Go view of `struct virtio_net_hdr` (Virtio 1.1
// §5.1.6.1). The driver always emits an all-zero header and ignores
// the received one.
type VirtioNetHdr struct {
	Flags      uint8
	GSOType    uint8
	HdrLen     uint16
	GSOSize    uint16
	CsumStart  uint16
	CsumOffset uint16
	NumBuffers uint16
}

// PrependVirtioNetHdr builds a 12-byte all-zero `virtio_net_hdr`
// followed by the Ethernet frame in `frame`. Returns the full
// per-descriptor buffer the driver passes to AddBuffer.
func PrependVirtioNetHdr(frame []byte) []byte {
	out := make([]byte, VirtioNetHdrSize+len(frame))
	// Header is already zero-initialised by make.
	copy(out[VirtioNetHdrSize:], frame)
	return out
}

// StripVirtioNetHdr returns the Ethernet frame embedded in a device-RX
// buffer of the given length. Returns nil + ErrFrameTooShort if the
// buffer is shorter than the header (a malformed RX).
func StripVirtioNetHdr(buf []byte) ([]byte, error) {
	if len(buf) < VirtioNetHdrSize {
		return nil, ErrFrameTooShort
	}
	return buf[VirtioNetHdrSize:], nil
}

// MAC6 is the 6-byte EUI-48 MAC address of one virtio-net device.
type MAC6 [6]byte

// IsZero reports whether the MAC is all-zero (used to detect a failed
// MAC read).
func (m MAC6) IsZero() bool {
	for _, b := range m {
		if b != 0 {
			return false
		}
	}
	return true
}

// String formats the MAC in standard "XX:XX:XX:XX:XX:XX" hex notation.
// Avoids fmt for a dep-light footprint.
func (m MAC6) String() string {
	const digits = "0123456789abcdef"
	var buf [17]byte
	for i := 0; i < 6; i++ {
		buf[i*3] = digits[(m[i]>>4)&0xF]
		buf[i*3+1] = digits[m[i]&0xF]
		if i < 5 {
			buf[i*3+2] = ':'
		}
	}
	return string(buf[:])
}

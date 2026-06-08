// go-virtio/net — driver core: feature negotiation + init sequence +
// TX / RX path.

package net

import (
	"github.com/go-virtio/common"
)

// AcceptedFeatures is the feature mask the driver negotiates ON:
//
//	VIRTIO_NET_F_MTU      (3)   device-provided MTU (REQUIRED by Apple VZ;
//	                            informational/no-op on QEMU+EDK2)
//	VIRTIO_NET_F_MAC      (5)   device MAC published in DeviceCfg
//	VIRTIO_NET_F_STATUS   (16)  link-up bit (informational)
//	VIRTIO_F_VERSION_1    (32)  modern transport (non-negotiable)
//
// Apple VZ live empirical narrow (2026-06-07) established that VZ
// clears FEATURES_OK and aborts the init unless VIRTIO_NET_F_MTU is
// acknowledged — ack'ing the bit costs us nothing on the driver side
// (we don't read the device-published `mtu` field) and unblocks the VZ
// cell. QEMU+EDK2 always offers this bit too, so accepting it is a
// no-op there.
//
// All other bits are masked OUT. If the device REQUIRES a bit we
// didn't ack, FEATURES_OK will fail to stick after we write it; the
// init sequence catches that and surfaces ErrFeaturesNotOK.
//
// NOTE: VIRTIO_NET_F_MRG_RXBUF (15) is NOT accepted. Without it the
// device places one packet per buffer (no chained descriptors on
// receive), which simplifies the RX path significantly.
const AcceptedFeatures uint64 = FeatureMTU | FeatureMAC | FeatureStatus | common.FeatureVersion1

// AcceptFeatures returns the negotiated feature mask: the intersection
// of what the device offers and what we accept. The caller writes this
// back via DriverFeature.
//
// We require VIRTIO_F_VERSION_1 — if the device doesn't offer it, we
// return ErrNotModernDevice and the init aborts. We require
// VIRTIO_NET_F_MAC because the probe needs the device-published MAC.
func AcceptFeatures(deviceFeatures uint64) (uint64, error) {
	if deviceFeatures&common.FeatureVersion1 == 0 {
		return 0, ErrNotModernDevice
	}
	negotiated := deviceFeatures & AcceptedFeatures
	if negotiated&FeatureMAC == 0 {
		return 0, ErrNoMACFeature
	}
	return negotiated, nil
}

// VirtioNet wraps one initialised virtio-net device. The caller holds
// this for the lifetime of the probe; the underlying virtqueue pages
// live as long as the supplied PageAllocator's lifetime contract.
type VirtioNet struct {
	// Cfg is the modern-transport handle (BARs + offsets + the
	// BARMemoryAccessor used for every register access).
	Cfg *common.ModernConfig

	// MAC is the device-published MAC address (Virtio 1.1 §5.1.4).
	// Read after FEATURES_OK at OpenVirtioNet completion.
	MAC MAC6

	// NegotiatedFeatures records what the driver-feature handshake
	// settled on. Exposed for diagnostic prints.
	NegotiatedFeatures uint64

	// transport is the underlying Transport — held so we can route
	// virtqueue allocations through the PageAllocator side.
	transport common.Transport

	// rxq / txq are the two virtqueues set up by OpenVirtioNet.
	rxq *common.Virtqueue
	txq *common.Virtqueue
}

// OpenVirtioNet drives the full bring-up of one virtio-net device:
//
//   1. Verify the PCI VID:DID is 1AF4:1041 (modern net).
//   2. InitModernConfig walks PCI caps + populates the BAR locators.
//   3. Reset → ACK → DRIVER status progression.
//   4. Read DeviceFeature, mask, write DriverFeature.
//   5. Set FEATURES_OK, verify it stuck.
//   6. Allocate + publish rxq (queue 0) + txq (queue 1).
//   7. DRIVER_OK status.
//   8. Read MAC from DeviceCfg.
//   9. Pre-post RxRingSize receive buffers + notify the device.
//
// On success the device is in DRIVER_OK state, the rxq is pre-posted
// with RxRingSize buffers, the txq is empty + ready, and the device
// MAC is set.
func OpenVirtioNet(t common.Transport) (*VirtioNet, error) {
	return OpenVirtioNetWithFeatures(t, AcceptedFeatures)
}

// OpenVirtioNetWithFeatures is the parameterised variant that takes a
// caller-supplied accepted-features mask. The override is applied
// AFTER the device's offered bitmap is read, so the negotiated mask is
// `deviceFeats & overrideAcceptedFeatures` (with FeatureVersion1 and
// FeatureMAC still enforced).
//
// Used by diagnostic narrows to test widening the accepted set
// (e.g. acknowledging Apple's private bits 28/29) without committing
// that to the production mask.
func OpenVirtioNetWithFeatures(t common.Transport, acceptedFeatures uint64) (*VirtioNet, error) {
	// Sanity-check this really is a modern virtio-net device.
	did, err := t.ReadConfig16(common.PCICfgDeviceID)
	if err != nil {
		return nil, err
	}
	if did != common.PCIDeviceIDModernNet {
		return nil, ErrInitWrongDeviceID
	}

	cfg, err := common.InitModernConfig(t)
	if err != nil {
		return nil, err
	}

	// Step 1: full reset (write 0 to DeviceStatus).
	if err := cfg.SetDeviceStatus(0); err != nil {
		return nil, err
	}
	// Spec §3.1.1: after reset, DeviceStatus reads back as 0. Some
	// firmware needs a moment; we don't sleep — fall through. The
	// resulting bytes are discarded after the read; we only care that
	// the read itself succeeds (firmware liveness check).
	if _, err := cfg.DeviceStatus(); err != nil {
		return nil, err
	}

	// Step 2: ACKNOWLEDGE.
	if err := cfg.SetDeviceStatus(common.StatusAcknowledge); err != nil {
		return nil, err
	}
	// Step 3: DRIVER.
	if err := cfg.SetDeviceStatus(common.StatusAcknowledge | common.StatusDriver); err != nil {
		return nil, err
	}

	// Step 4: read DeviceFeature, mask, write DriverFeature.
	deviceFeats, err := cfg.DeviceFeatures64()
	if err != nil {
		return nil, err
	}
	if deviceFeats&common.FeatureVersion1 == 0 {
		return nil, ErrNotModernDevice
	}
	negotiated := deviceFeats & acceptedFeatures
	if negotiated&FeatureMAC == 0 {
		return nil, ErrNoMACFeature
	}
	if err := cfg.SetDriverFeatures64(negotiated); err != nil {
		return nil, err
	}

	// Step 5: FEATURES_OK + verify the device accepted our subset.
	if err := cfg.SetDeviceStatus(common.StatusAcknowledge | common.StatusDriver | common.StatusFeaturesOK); err != nil {
		return nil, err
	}
	status, err := cfg.DeviceStatus()
	if err != nil {
		return nil, err
	}
	if status&common.StatusFeaturesOK == 0 {
		return nil, ErrFeaturesNotOK
	}

	// Step 6: queue setup.
	rxq, err := setupQueue(cfg, t, RxQueueIdx, RxRingSize)
	if err != nil {
		return nil, err
	}
	txq, err := setupQueue(cfg, t, TxQueueIdx, TxRingSize)
	if err != nil {
		return nil, err
	}

	// Step 7: DRIVER_OK.
	if err := cfg.SetDeviceStatus(common.StatusAcknowledge | common.StatusDriver | common.StatusFeaturesOK | common.StatusDriverOK); err != nil {
		return nil, err
	}

	// Step 8: read MAC (6 bytes from DeviceCfg @ offset 0).
	var mac MAC6
	for i := uint32(0); i < MACLen; i++ {
		b, err := cfg.DeviceCfgRead8(i)
		if err != nil {
			return nil, err
		}
		mac[i] = b
	}
	if mac.IsZero() {
		// Some firmware fills MAC lazily after DRIVER_OK; we don't
		// retry. QEMU and VZ both publish the MAC by the time we get
		// here.
		return nil, ErrMACReadFailed
	}

	v := &VirtioNet{
		Cfg:                cfg,
		MAC:                mac,
		NegotiatedFeatures: negotiated,
		transport:          t,
		rxq:                rxq,
		txq:                txq,
	}

	// Pre-post N receive buffers so the device has somewhere to land
	// incoming frames.
	if err := v.fillRxRing(); err != nil {
		return nil, err
	}
	// Notify the device that the rxq has buffers available — VZ in
	// particular won't deliver frames otherwise.
	if err := cfg.NotifyQueue(RxQueueIdx, rxq.NotifyOff); err != nil {
		return nil, err
	}

	return v, nil
}

// setupQueue performs the per-queue init: select, read max-size, write
// our size (= min(desired, max), rounded down to a power of two),
// allocate the Virtqueue, write its descriptor/avail/used physical
// addresses, enable.
func setupQueue(cfg *common.ModernConfig, t common.Transport, queueIdx uint16, desiredSize uint16) (*common.Virtqueue, error) {
	if err := cfg.SelectQueue(queueIdx); err != nil {
		return nil, err
	}
	maxSize, err := cfg.QueueSize()
	if err != nil {
		return nil, err
	}
	if maxSize == 0 {
		// Device doesn't have this queue; spec says the driver should
		// not use it.
		return nil, ErrQueueNotAvailable
	}
	size := desiredSize
	if size > maxSize {
		size = maxSize
	}
	// Round size DOWN to a power of two; some QEMU versions report
	// non-power-of-two QueueSize on legacy queues.
	for size&(size-1) != 0 {
		size &= size - 1
	}
	if size == 0 {
		return nil, common.ErrInvalidQueueSize
	}
	if err := cfg.SetQueueSize(size); err != nil {
		return nil, err
	}
	notifyOff, err := cfg.QueueNotifyOff()
	if err != nil {
		return nil, err
	}
	q, err := common.NewVirtqueue(t, size, queueIdx, notifyOff)
	if err != nil {
		return nil, err
	}
	descAddr := q.BasePhys + uint64(q.Layout.DescTableOffset)
	availAddr := q.BasePhys + uint64(q.Layout.AvailRingOffset)
	usedAddr := q.BasePhys + uint64(q.Layout.UsedRingOffset)
	if err := cfg.SetQueueDesc(descAddr); err != nil {
		return nil, err
	}
	if err := cfg.SetQueueDriver(availAddr); err != nil {
		return nil, err
	}
	if err := cfg.SetQueueDevice(usedAddr); err != nil {
		return nil, err
	}
	if err := cfg.SetQueueEnable(1); err != nil {
		return nil, err
	}
	return q, nil
}

// RxQueue / TxQueue expose the per-direction *common.Virtqueue
// handles. Read-only accessors so callers can inspect ring state for
// diagnostic dumps; the fields themselves stay unexported.
func (v *VirtioNet) RxQueue() *common.Virtqueue { return v.rxq }

// TxQueue returns the TX virtqueue handle.
func (v *VirtioNet) TxQueue() *common.Virtqueue { return v.txq }

// fillRxRing posts RxRingSize receive buffers on the rxq. Each buffer
// is `VirtioNetHdrSize + MaxFrameSize` bytes (the device writes the
// virtio header first, then the Ethernet frame).
func (v *VirtioNet) fillRxRing() error {
	for i := uint16(0); i < v.rxq.Layout.Size; i++ {
		phys, mem, err := v.transport.AllocatePages(1)
		if err != nil {
			return err
		}
		if phys == 0 {
			return common.ErrAllocReturnedZero
		}
		// The buffer is the first (VirtioNetHdrSize + MaxFrameSize)
		// bytes of the allocated page. The remaining 4096 - that bytes
		// are unused — the alternative is allocating sub-page chunks
		// which the PageAllocator interface doesn't promise. Future
		// allocators may add a "small chunk" path; for now one page
		// per RX buffer is correct + simple.
		bufLen := uint32(VirtioNetHdrSize + MaxFrameSize)
		if uint64(bufLen) > uint64(len(mem)) {
			return ErrBufferTooSmall
		}
		// writable=true ⇒ VIRTQ_DESC_F_WRITE set.
		addr := uintptr(phys) // identity-mapped on supported hosts
		_, _ = mem, addr      // mem retained only via the descriptor publication
		if _, err := v.rxq.AddBuffer(addr, phys, bufLen, true); err != nil {
			return err
		}
	}
	return nil
}

// TransmitFrame copies a virtio_net_hdr + the Ethernet `frame` into a
// fresh DMA-visible buffer, enqueues it on the txq, notifies the
// device, and polls the used ring for completion.
//
// Polls for up to `TxPollIterations` iterations. On QEMU the
// round-trip is usually < 1 ms; on VZ similar. Returns
// ErrTransmitTimeout if the budget exhausts.
func (v *VirtioNet) TransmitFrame(frame []byte) error {
	totalLen := VirtioNetHdrSize + len(frame)
	phys, mem, err := v.transport.AllocatePages(1)
	if err != nil {
		return err
	}
	if phys == 0 {
		return common.ErrAllocReturnedZero
	}
	if totalLen > len(mem) {
		return ErrBufferTooSmall
	}
	// Header is already zero-initialised by AllocatePages contract.
	copy(mem[VirtioNetHdrSize:], frame)

	if _, err := v.txq.AddBuffer(uintptr(phys), phys, uint32(totalLen), false); err != nil {
		return err
	}
	if err := v.Cfg.NotifyQueue(TxQueueIdx, v.txq.NotifyOff); err != nil {
		return err
	}
	for spin := 0; spin < TxPollIterations; spin++ {
		gotIdx, _, ok := v.txq.PollUsed()
		if !ok {
			continue
		}
		_ = v.txq.Reclaim(gotIdx)
		return nil
	}
	return ErrTransmitTimeout
}

// ReceiveFrame polls the rxq for one new frame. Returns the Ethernet
// payload (header stripped) on success, or ErrReceiveTimeout if no
// frame arrives within `pollIterations` busy-spin cycles.
//
// The returned slice is a fresh copy of the descriptor's DMA buffer —
// safe to retain after this call returns (and after the descriptor is
// reclaimed and refilled).
func (v *VirtioNet) ReceiveFrame(pollIterations int) ([]byte, error) {
	for spin := 0; spin < pollIterations; spin++ {
		descIdx, length, ok := v.rxq.PollUsed()
		if !ok {
			continue
		}
		buf := v.rxq.Buffers[descIdx]
		// Read the device-published byte view of the buffer. Caller
		// will copy out so we don't have to keep the descriptor
		// pinned.
		raw := readBufferBytes(buf.Addr, int(length))
		out := make([]byte, len(raw))
		copy(out, raw)
		_ = v.rxq.Reclaim(descIdx)
		// Re-post the same buffer (it's still allocated) so the device
		// has somewhere to land the next frame.
		if _, err := v.rxq.AddBuffer(buf.Addr, buf.Phys, buf.Len, true); err != nil {
			// Re-post failed; we're degraded but the captured frame is
			// still good to return.
			_ = err
		}
		if err := v.Cfg.NotifyQueue(RxQueueIdx, v.rxq.NotifyOff); err != nil {
			_ = err
		}
		return StripVirtioNetHdr(out)
	}
	return nil, ErrReceiveTimeout
}

// TxPollIterations is the default polling budget for TransmitFrame.
// Empirically large enough on QEMU+EDK2 and Apple VZ; far from the
// firmware's actual round-trip but a sensible upper bound for the
// busy-poll model the driver uses.
const TxPollIterations = 200000

// Sentinel errors for the virtio-net path. All exported so callers
// can branch + format them.
var (
	ErrFrameTooShort     = commonNetError("go-virtio/net: RX buffer shorter than virtio_net_hdr (12 bytes)")
	ErrNotModernDevice   = commonNetError("go-virtio/net: device doesn't offer VIRTIO_F_VERSION_1 (legacy-only)")
	ErrNoMACFeature      = commonNetError("go-virtio/net: device doesn't offer VIRTIO_NET_F_MAC")
	ErrFeaturesNotOK     = commonNetError("go-virtio/net: FEATURES_OK status bit didn't stick after DriverFeature write")
	ErrMACReadFailed     = commonNetError("go-virtio/net: MAC read returned all-zero (bounds-check failure or unsupported device)")
	ErrInitWrongDeviceID = commonNetError("go-virtio/net: PCI device ID is not 0x1041 (modern net device)")
	ErrQueueNotAvailable = commonNetError("go-virtio/net: device reports QueueSize=0 for required queue")
	ErrTransmitTimeout   = commonNetError("go-virtio/net: TX poll timeout (device did not return descriptor)")
	ErrReceiveTimeout    = commonNetError("go-virtio/net: RX poll timeout (no frame received within budget)")
	ErrBufferTooSmall    = commonNetError("go-virtio/net: PageAllocator returned a chunk smaller than VirtioNetHdrSize + MaxFrameSize")
)

// commonNetError is the package's tiny sentinel-error type — same
// pattern as go-virtio/common.commonError.
type commonNetError string

// Error implements the `error` interface.
func (e commonNetError) Error() string { return string(e) }

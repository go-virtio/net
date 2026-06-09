// SPDX-License-Identifier: BSD-3-Clause
//
// Targeted error-branch coverage: drives every `if err != nil { return …
// }` and every sentinel-error site in OpenVirtioNetWithFeatures /
// setupQueue / fillRxRing / TransmitFrame / ReceiveFrame.
//
// Pattern: tapTransport wraps a working *fakeDevice and lets the test
// inject a one-shot failure into a specific method (or, for the
// COMMON_CFG accessors, a specific (bar, offset) pair). One subtest per
// uncovered branch. All branches assert on the expected sentinel error
// from this package or go-virtio/common where applicable.

package net

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/go-virtio/common"
)

// tapTransport delegates every Transport method to an embedded
// *fakeDevice. A test sets exactly one of the failOn* fields to inject a
// failure at the matching call site. The failure is one-shot: after
// firing once, subsequent calls fall through to the underlying
// fakeDevice. That keeps tests focused — the driver bails on the first
// error so further intercepts wouldn't run anyway, but the one-shot
// guarantee makes the model explicit.
type tapTransport struct {
	base *fakeDevice

	// PCIConfigReader hooks. failOnReadConfig16Off triggers when ReadConfig16
	// is called with that offset.
	failOnReadConfig16Off uint8
	failOnReadConfig16    bool

	// BARMemoryAccessor hooks: each (bar, off) target fires once. The
	// "Once" sentinel guards against firing on multiple calls.
	failOnRead8     *barTarget
	failOnRead16    *barTarget
	failOnRead32    *barTarget
	failOnWrite8    *barTarget
	failOnWrite16   *barTarget
	failOnWrite32   *barTarget
	failOnWrite64   *barTarget

	// PageAllocator: nth (1-indexed) call returns err. After firing once
	// (or if 0), normal path.
	failAllocOnNthCall int
	allocCallCount     int

	// shortAllocBytes truncates the returned mem to this size on the
	// indicated call (so callers see `bufLen > len(mem)`).
	shortAllocOnNthCall int
	shortAllocBytes     int
}

// barTarget identifies a single BAR access. fired guards single-shot.
type barTarget struct {
	bar   uint8
	off   uint64
	fired bool
}

func (b *barTarget) match(bar uint8, off uint64) bool {
	if b == nil || b.fired {
		return false
	}
	if b.bar == bar && b.off == off {
		b.fired = true
		return true
	}
	return false
}

// PCIConfigReader
func (t *tapTransport) ReadConfig8(o uint8) (uint8, error) { return t.base.ReadConfig8(o) }
func (t *tapTransport) ReadConfig16(o uint8) (uint16, error) {
	if t.failOnReadConfig16 && o == t.failOnReadConfig16Off {
		t.failOnReadConfig16 = false
		return 0, errors.New("inject: ReadConfig16")
	}
	return t.base.ReadConfig16(o)
}
func (t *tapTransport) ReadConfig32(o uint8) (uint32, error) { return t.base.ReadConfig32(o) }

// BARMemoryAccessor
func (t *tapTransport) Read8(bar uint8, off uint64) (uint8, error) {
	if t.failOnRead8.match(bar, off) {
		return 0, errors.New("inject: Read8")
	}
	return t.base.Read8(bar, off)
}
func (t *tapTransport) Read16(bar uint8, off uint64) (uint16, error) {
	if t.failOnRead16.match(bar, off) {
		return 0, errors.New("inject: Read16")
	}
	return t.base.Read16(bar, off)
}
func (t *tapTransport) Read32(bar uint8, off uint64) (uint32, error) {
	if t.failOnRead32.match(bar, off) {
		return 0, errors.New("inject: Read32")
	}
	return t.base.Read32(bar, off)
}
func (t *tapTransport) Read64(bar uint8, off uint64) (uint64, error) {
	return t.base.Read64(bar, off)
}
func (t *tapTransport) Write8(bar uint8, off uint64, v uint8) error {
	if t.failOnWrite8.match(bar, off) {
		return errors.New("inject: Write8")
	}
	return t.base.Write8(bar, off, v)
}
func (t *tapTransport) Write16(bar uint8, off uint64, v uint16) error {
	if t.failOnWrite16.match(bar, off) {
		return errors.New("inject: Write16")
	}
	return t.base.Write16(bar, off, v)
}
func (t *tapTransport) Write32(bar uint8, off uint64, v uint32) error {
	if t.failOnWrite32.match(bar, off) {
		return errors.New("inject: Write32")
	}
	return t.base.Write32(bar, off, v)
}
func (t *tapTransport) Write64(bar uint8, off uint64, v uint64) error {
	if t.failOnWrite64.match(bar, off) {
		return errors.New("inject: Write64")
	}
	return t.base.Write64(bar, off, v)
}

// PageAllocator
func (t *tapTransport) AllocatePages(count int) (uint64, []byte, error) {
	t.allocCallCount++
	if t.failAllocOnNthCall != 0 && t.allocCallCount == t.failAllocOnNthCall {
		return 0, nil, errors.New("inject: AllocatePages")
	}
	phys, mem, err := t.base.AllocatePages(count)
	if err != nil {
		return phys, mem, err
	}
	if t.shortAllocOnNthCall != 0 && t.allocCallCount == t.shortAllocOnNthCall {
		// Truncate the slice so callers see the "too small" branch.
		mem = mem[:t.shortAllocBytes]
	}
	return phys, mem, err
}

// newTap returns a tapTransport wired to a VZ-shape fakeDevice (MAC +
// MTU + STATUS + VERSION_1, valid MAC). Callers tweak the device or
// flip a tap.failOn* before calling OpenVirtioNet.
func newTap() (*tapTransport, *fakeDevice) {
	mac := [6]byte{1, 2, 3, 4, 5, 6}
	d := newFakeDevice(FeatureMAC|FeatureMTU|FeatureStatus|common.FeatureVersion1, mac)
	return &tapTransport{base: d}, d
}

// -- OpenVirtioNetWithFeatures error branches --------------------------

func TestOpenVirtioNet_ErrorBranches(t *testing.T) {
	t.Run("ReadConfig16 PCICfgDeviceID fails", func(t *testing.T) {
		tap, _ := newTap()
		tap.failOnReadConfig16 = true
		tap.failOnReadConfig16Off = common.PCICfgDeviceID
		_, err := OpenVirtioNet(tap)
		if err == nil || err.Error() != "inject: ReadConfig16" {
			t.Errorf("got %v, want injected ReadConfig16 error", err)
		}
	})

	t.Run("InitModernConfig fails (capability list truncated)", func(t *testing.T) {
		tap, d := newTap()
		// Clear the PCIStatusCapabilityList bit so InitModernConfig
		// returns an error.
		binary.LittleEndian.PutUint16(d.cfg[6:], 0)
		_, err := OpenVirtioNet(tap)
		if err == nil {
			t.Error("expected InitModernConfig error")
		}
	})

	t.Run("Reset (SetDeviceStatus 0) fails", func(t *testing.T) {
		tap, _ := newTap()
		// First Write8 to CfgDeviceStatus is the reset write.
		tap.failOnWrite8 = &barTarget{bar: 0, off: common.CfgDeviceStatus}
		_, err := OpenVirtioNet(tap)
		if err == nil || err.Error() != "inject: Write8" {
			t.Errorf("got %v", err)
		}
	})

	t.Run("DeviceStatus liveness read fails", func(t *testing.T) {
		tap, _ := newTap()
		tap.failOnRead8 = &barTarget{bar: 0, off: common.CfgDeviceStatus}
		_, err := OpenVirtioNet(tap)
		if err == nil || err.Error() != "inject: Read8" {
			t.Errorf("got %v", err)
		}
	})

	t.Run("ACKNOWLEDGE SetDeviceStatus fails", func(t *testing.T) {
		tap, _ := newTap()
		// 2nd Write8(CfgDeviceStatus): reset, ACK.
		nthTap := &nthWrite8Tap{tapTransport: tap, targetBar: 0, targetOff: common.CfgDeviceStatus, failOnNth: 2}
		_, err := OpenVirtioNet(nthTap)
		if err == nil {
			t.Error("expected ACKNOWLEDGE write error")
		}
	})

	t.Run("DRIVER SetDeviceStatus fails", func(t *testing.T) {
		tap, _ := newTap()
		nthTap := &nthWrite8Tap{tapTransport: tap, targetBar: 0, targetOff: common.CfgDeviceStatus, failOnNth: 3}
		_, err := OpenVirtioNet(nthTap)
		if err == nil {
			t.Error("expected DRIVER write error")
		}
	})

	t.Run("DeviceFeatures64 read fails", func(t *testing.T) {
		tap, _ := newTap()
		// DeviceFeatures64 issues a Write32(CfgDeviceFeatureSelect=0)
		// then Read32(CfgDeviceFeature). Fail the read.
		tap.failOnRead32 = &barTarget{bar: 0, off: common.CfgDeviceFeature}
		_, err := OpenVirtioNet(tap)
		if err == nil || err.Error() != "inject: Read32" {
			t.Errorf("got %v", err)
		}
	})

	t.Run("SetDriverFeatures64 write fails", func(t *testing.T) {
		tap, _ := newTap()
		// SetDriverFeatures64 issues writes to CfgDriverFeatureSelect +
		// CfgDriverFeature. Fail the first CfgDriverFeature write.
		tap.failOnWrite32 = &barTarget{bar: 0, off: common.CfgDriverFeature}
		_, err := OpenVirtioNet(tap)
		if err == nil || err.Error() != "inject: Write32" {
			t.Errorf("got %v", err)
		}
	})

	t.Run("FEATURES_OK SetDeviceStatus fails", func(t *testing.T) {
		tap, _ := newTap()
		// The FEATURES_OK write is the 4th Write8 to CfgDeviceStatus
		// (reset, ACK, DRIVER, FEATURES_OK).
		nthTap := &nthWrite8Tap{tapTransport: tap, targetBar: 0, targetOff: common.CfgDeviceStatus, failOnNth: 4}
		_, err := OpenVirtioNet(nthTap)
		if err == nil {
			t.Error("expected FEATURES_OK write error")
		}
	})

	t.Run("post-FEATURES_OK DeviceStatus read fails", func(t *testing.T) {
		tap, _ := newTap()
		// After FEATURES_OK is written, the driver issues a Read8 to
		// verify the bit stuck. Fail the SECOND Read8(CfgDeviceStatus):
		// the first is the liveness check after reset.
		nthTap := &nthRead8Tap{tapTransport: tap, targetBar: 0, targetOff: common.CfgDeviceStatus, failOnNth: 2}
		_, err := OpenVirtioNet(nthTap)
		if err == nil {
			t.Error("expected post-FEATURES_OK Read8 error")
		}
	})

	t.Run("TX queue setupQueue fails", func(t *testing.T) {
		tap, _ := newTap()
		// SelectQueue(TxQueueIdx=1) is the 2nd Write16(CfgQueueSelect).
		nthTap := &nthWrite16Tap{tapTransport: tap, targetBar: 0, targetOff: common.CfgQueueSelect, failOnNth: 2}
		_, err := OpenVirtioNet(nthTap)
		if err == nil {
			t.Error("expected TX setupQueue error")
		}
	})

	t.Run("DRIVER_OK SetDeviceStatus fails", func(t *testing.T) {
		tap, _ := newTap()
		// 5th Write8(CfgDeviceStatus): reset, ACK, DRIVER, FEATURES_OK,
		// DRIVER_OK.
		nthTap := &nthWrite8Tap{tapTransport: tap, targetBar: 0, targetOff: common.CfgDeviceStatus, failOnNth: 5}
		_, err := OpenVirtioNet(nthTap)
		if err == nil {
			t.Error("expected DRIVER_OK write error")
		}
	})

	t.Run("MAC DeviceCfgRead8 fails", func(t *testing.T) {
		tap, _ := newTap()
		// DeviceCfg is at BAR=0 offset 0x8000. Fail the first MAC read.
		tap.failOnRead8 = &barTarget{bar: 0, off: 0x8000}
		_, err := OpenVirtioNet(tap)
		if err == nil || err.Error() != "inject: Read8" {
			t.Errorf("got %v", err)
		}
	})

	t.Run("RX notify after fillRxRing fails", func(t *testing.T) {
		tap, _ := newTap()
		// NotifyQueue's BAR write uses Write32 when notify_off_multiplier >= 4
		// (our cfg space publishes multiplier=4). RX notify offset is 0x1000.
		tap.failOnWrite32 = &barTarget{bar: 0, off: 0x1000}
		_, err := OpenVirtioNet(tap)
		if err == nil || err.Error() != "inject: Write32" {
			t.Errorf("got %v", err)
		}
	})
}

// -- setupQueue error branches -----------------------------------------

func TestSetupQueue_ErrorBranches(t *testing.T) {
	t.Run("SelectQueue fails", func(t *testing.T) {
		tap, _ := newTap()
		// First Write16(CfgQueueSelect) is the RX SelectQueue.
		tap.failOnWrite16 = &barTarget{bar: 0, off: common.CfgQueueSelect}
		_, err := OpenVirtioNet(tap)
		if err == nil || err.Error() != "inject: Write16" {
			t.Errorf("got %v", err)
		}
	})

	t.Run("QueueSize read fails", func(t *testing.T) {
		tap, _ := newTap()
		tap.failOnRead16 = &barTarget{bar: 0, off: common.CfgQueueSize}
		_, err := OpenVirtioNet(tap)
		if err == nil || err.Error() != "inject: Read16" {
			t.Errorf("got %v", err)
		}
	})

	t.Run("size > maxSize clamp branch", func(t *testing.T) {
		tap, d := newTap()
		// RxRingSize=16. Cap the device max to 4 — the clamp triggers.
		d.qsize[0] = 4
		_, err := OpenVirtioNet(tap)
		if err != nil {
			t.Errorf("expected success with clamp; got %v", err)
		}
	})

	t.Run("non-power-of-two QueueSize round-down loop", func(t *testing.T) {
		tap, d := newTap()
		// maxSize=3 -> size=3; loop runs once: 3&2=2, exits at 2.
		d.qsize[0] = 3
		d.qsize[1] = 3
		_, err := OpenVirtioNet(tap)
		if err != nil {
			t.Errorf("expected success after round-down; got %v", err)
		}
	})

	t.Run("SetQueueSize write fails", func(t *testing.T) {
		tap, _ := newTap()
		tap.failOnWrite16 = &barTarget{bar: 0, off: common.CfgQueueSize}
		_, err := OpenVirtioNet(tap)
		if err == nil || err.Error() != "inject: Write16" {
			t.Errorf("got %v", err)
		}
	})

	t.Run("QueueNotifyOff read fails", func(t *testing.T) {
		tap, _ := newTap()
		tap.failOnRead16 = &barTarget{bar: 0, off: common.CfgQueueNotifyOff}
		_, err := OpenVirtioNet(tap)
		if err == nil || err.Error() != "inject: Read16" {
			t.Errorf("got %v", err)
		}
	})

	t.Run("NewVirtqueue alloc fail (RX queue)", func(t *testing.T) {
		tap, _ := newTap()
		// First AllocatePages call is the RX virtqueue backing pages.
		tap.failAllocOnNthCall = 1
		_, err := OpenVirtioNet(tap)
		if err == nil {
			t.Error("expected RX NewVirtqueue alloc error")
		}
	})

	t.Run("SetQueueDesc write fails", func(t *testing.T) {
		tap, _ := newTap()
		tap.failOnWrite64 = &barTarget{bar: 0, off: common.CfgQueueDesc}
		_, err := OpenVirtioNet(tap)
		if err == nil || err.Error() != "inject: Write64" {
			t.Errorf("got %v", err)
		}
	})

	t.Run("SetQueueDriver write fails", func(t *testing.T) {
		tap, _ := newTap()
		tap.failOnWrite64 = &barTarget{bar: 0, off: common.CfgQueueDriver}
		_, err := OpenVirtioNet(tap)
		if err == nil || err.Error() != "inject: Write64" {
			t.Errorf("got %v", err)
		}
	})

	t.Run("SetQueueDevice write fails", func(t *testing.T) {
		tap, _ := newTap()
		tap.failOnWrite64 = &barTarget{bar: 0, off: common.CfgQueueDevice}
		_, err := OpenVirtioNet(tap)
		if err == nil || err.Error() != "inject: Write64" {
			t.Errorf("got %v", err)
		}
	})

	t.Run("SetQueueEnable write fails", func(t *testing.T) {
		tap, _ := newTap()
		tap.failOnWrite16 = &barTarget{bar: 0, off: common.CfgQueueEnable}
		_, err := OpenVirtioNet(tap)
		if err == nil || err.Error() != "inject: Write16" {
			t.Errorf("got %v", err)
		}
	})
}

// -- fillRxRing error branches -----------------------------------------

func TestFillRxRing_ErrorBranches(t *testing.T) {
	t.Run("buffer too small (allocator returns truncated mem)", func(t *testing.T) {
		tap, _ := newTap()
		// fillRxRing's AllocatePages is call #3 (after RX/TX virtqueue
		// backing pages).
		tap.shortAllocOnNthCall = 3
		tap.shortAllocBytes = 8 // way under VirtioNetHdrSize+MaxFrameSize
		_, err := OpenVirtioNet(tap)
		if !errors.Is(err, ErrBufferTooSmall) {
			t.Errorf("got %v, want ErrBufferTooSmall", err)
		}
	})

	t.Run("AddBuffer fails (queue full)", func(t *testing.T) {
		tap, d := newTap()
		// Round qsize down to 2 so the queue is small. Make the queue
		// size lie about the layout — actually simplest: shrink qsize
		// from 32 to 2. fillRxRing posts RxRingSize=16 buffers; after
		// the queue capacity (2) is exhausted, AddBuffer returns
		// ErrQueueFull. BUT the loop only iterates `rxq.Layout.Size`
		// times — which is also 2 in that case, so it never overflows.
		//
		// Force the overflow by setting the rxq.Layout.Size bigger than
		// the descriptor capacity. The cleanest trick: make AddBuffer
		// fail by exhausting the descriptor slots via post-call
		// patching. We patch v.rxq.Layout.Size after the bring-up... but
		// fillRxRing is inside OpenVirtioNet.
		//
		// Alternative: short-circuit via the allocator. fillRxRing
		// allocates page #3+ for the RX buffers; if AddBuffer is the
		// only un-tested branch left, the cleanest approach is to
		// post-construct a VirtioNet with a saturated rxq and call
		// fillRxRing directly. Do that here.
		_ = d
		v, err := OpenVirtioNet(tap)
		if err != nil {
			t.Fatalf("bring-up: %v", err)
		}
		// Saturate the rxq: every buffer InUse=true. Then bump the
		// Layout.Size logically by re-running fillRxRing — but the loop
		// will iterate Layout.Size times and EACH AddBuffer will fail
		// because all slots are InUse. Result: first iteration's
		// AddBuffer returns ErrQueueFull → fillRxRing returns that
		// error.
		for i := range v.rxq.Buffers {
			v.rxq.Buffers[i].InUse = true
		}
		if err := v.fillRxRing(); !errors.Is(err, common.ErrQueueFull) {
			t.Errorf("got %v, want ErrQueueFull", err)
		}
	})
}

// -- TransmitFrame error branches --------------------------------------

func TestTransmitFrame_ErrorBranches(t *testing.T) {
	t.Run("buffer too small", func(t *testing.T) {
		tap, _ := newTap()
		v, err := OpenVirtioNet(tap)
		if err != nil {
			t.Fatalf("bring-up: %v", err)
		}
		// Next AllocatePages call (TransmitFrame's) should return
		// truncated mem. We've used some calls already during bring-up;
		// reset the counter for clarity then trigger on the very next.
		tap.shortAllocOnNthCall = tap.allocCallCount + 1
		tap.shortAllocBytes = 4
		err = v.TransmitFrame([]byte{1, 2, 3})
		if !errors.Is(err, ErrBufferTooSmall) {
			t.Errorf("got %v, want ErrBufferTooSmall", err)
		}
	})

	t.Run("AddBuffer fails (queue full)", func(t *testing.T) {
		tap, _ := newTap()
		v, err := OpenVirtioNet(tap)
		if err != nil {
			t.Fatalf("bring-up: %v", err)
		}
		// Saturate the txq descriptor slots so AddBuffer returns
		// ErrQueueFull.
		for i := range v.txq.Buffers {
			v.txq.Buffers[i].InUse = true
		}
		if err := v.TransmitFrame([]byte{1, 2, 3}); !errors.Is(err, common.ErrQueueFull) {
			t.Errorf("got %v, want ErrQueueFull", err)
		}
	})

	t.Run("NotifyQueue fails", func(t *testing.T) {
		tap, _ := newTap()
		v, err := OpenVirtioNet(tap)
		if err != nil {
			t.Fatalf("bring-up: %v", err)
		}
		// Arm a one-shot Write32 failure on the TX notify offset (RX is
		// 0x1000, TX is 0x1000 + 1*4 = 0x1004 with multiplier=4).
		tap.failOnWrite32 = &barTarget{bar: 0, off: 0x1004}
		err = v.TransmitFrame([]byte{1, 2, 3})
		if err == nil || err.Error() != "inject: Write32" {
			t.Errorf("got %v, want injected Write32", err)
		}
	})
}

// -- ReceiveFrame error branches --------------------------------------
//
// The ReceiveFrame implementation INTENTIONALLY swallows AddBuffer and
// NotifyQueue errors after a successful frame extraction (the captured
// frame is still good to return). To cover those lines we drive a frame
// through and inject a failure in the re-post path.

func TestReceiveFrame_ErrorBranches(t *testing.T) {
	t.Run("AddBuffer re-post fails (swallowed)", func(t *testing.T) {
		// Strategy:
		//   - Bring up normally; the rxq starts with Layout.Size=32 and
		//     all 16 fillRxRing slots InUse (slots 0..15).
		//   - Manually publish a used-ring entry with descIdx in range
		//     of Buffers (say 5).
		//   - Saturate ALL Buffers InUse.
		//   - Shrink Layout.Size to 1.
		//   - Reclaim(5) returns ErrInvalidIdx because 5 >= Size=1, so
		//     no Buffers slot is freed.
		//   - AddBuffer loops over i := 0; i < 1; Buffers[0].InUse=true
		//     → returns ErrQueueFull.
		// ReceiveFrame swallows the error (kept frame still good).
		mac := [6]byte{1, 2, 3, 4, 5, 6}
		base := newFakeDevice(FeatureMAC|FeatureMTU|FeatureStatus|common.FeatureVersion1, mac)
		v, err := OpenVirtioNet(base)
		if err != nil {
			t.Fatalf("OpenVirtioNet: %v", err)
		}
		// Saturate every slot InUse.
		for i := range v.rxq.Buffers {
			v.rxq.Buffers[i].InUse = true
		}
		// Publish a used-ring entry: descIdx=5 at slot 0, then bump
		// usedIdx to 1. The used ring lives at q.BasePhys +
		// UsedRingOffset; the rxq's qdriver/qdevice were written
		// during bring-up. Use the rxq layout directly.
		usedSlice := readBufferBytes(uintptr(v.rxq.BasePhys+uint64(v.rxq.Layout.UsedRingOffset)), 4+8)
		if usedSlice == nil {
			t.Fatal("could not get usedSlice")
		}
		binary.LittleEndian.PutUint32(usedSlice[4:8], 5)   // descIdx
		binary.LittleEndian.PutUint32(usedSlice[8:12], 16) // length
		binary.LittleEndian.PutUint16(usedSlice[2:4], 1)   // bump usedIdx
		// Shrink the logical queue size to 1 so Reclaim(5) fails AND
		// AddBuffer's scan only inspects slot 0 (which is InUse).
		v.rxq.Layout.Size = 1
		// Drive ReceiveFrame. Buffers[5].Addr is whatever fillRxRing
		// originally wrote; readBufferBytes will succeed on it.
		frame, err := v.ReceiveFrame(10)
		if err != nil {
			t.Errorf("ReceiveFrame should swallow re-post AddBuffer error: got %v", err)
		}
		if frame == nil {
			t.Error("expected non-nil frame even with swallowed error")
		}
	})

	t.Run("NotifyQueue re-post fails (swallowed)", func(t *testing.T) {
		mac := [6]byte{1, 2, 3, 4, 5, 6}
		base := newFakeDevice(FeatureMAC|FeatureMTU|FeatureStatus|common.FeatureVersion1, mac)
		rx := &rxFakeDevice{
			fakeDevice:     base,
			rxFramesNeeded: 1,
		}
		// Wrap rx in a tap-shaped passthrough so we can inject a
		// Write16 failure on the RX notify register. rxFakeDevice
		// itself isn't a tapTransport — write a dedicated wrapper.
		w := &notifyFailRx{rxFakeDevice: rx, failOff: 0x1000}
		v, err := OpenVirtioNet(w)
		if err != nil {
			t.Fatalf("OpenVirtioNet: %v", err)
		}
		// The bring-up's RX notify already fired — toggle the failure
		// AFTER bring-up so the next NotifyQueue (inside ReceiveFrame)
		// is what fails.
		w.armed = true
		frame, err := v.ReceiveFrame(10)
		if err != nil {
			t.Errorf("ReceiveFrame should swallow re-post NotifyQueue error: got %v", err)
		}
		if len(frame) != 4 {
			t.Errorf("frame length: got %d, want 4", len(frame))
		}
	})
}

// -- helpers ----------------------------------------------------------

// nthWrite8Tap fails the Nth Write8 call to (targetBar, targetOff). All
// other Write8 calls pass through. Used to target the 2nd / 3rd / 4th
// CfgDeviceStatus write distinctly.
type nthWrite8Tap struct {
	*tapTransport
	targetBar uint8
	targetOff uint64
	failOnNth int
	count     int
}

func (n *nthWrite8Tap) Write8(bar uint8, off uint64, v uint8) error {
	if bar == n.targetBar && off == n.targetOff {
		n.count++
		if n.count == n.failOnNth {
			return errors.New("inject: nth Write8")
		}
	}
	return n.tapTransport.Write8(bar, off, v)
}

// nthRead8Tap fails the Nth Read8 call to (targetBar, targetOff).
type nthRead8Tap struct {
	*tapTransport
	targetBar uint8
	targetOff uint64
	failOnNth int
	count     int
}

func (n *nthRead8Tap) Read8(bar uint8, off uint64) (uint8, error) {
	if bar == n.targetBar && off == n.targetOff {
		n.count++
		if n.count == n.failOnNth {
			return 0, errors.New("inject: nth Read8")
		}
	}
	return n.tapTransport.Read8(bar, off)
}

// nthWrite16Tap fails the Nth Write16 to (targetBar, targetOff).
type nthWrite16Tap struct {
	*tapTransport
	targetBar uint8
	targetOff uint64
	failOnNth int
	count     int
}

func (n *nthWrite16Tap) Write16(bar uint8, off uint64, v uint16) error {
	if bar == n.targetBar && off == n.targetOff {
		n.count++
		if n.count == n.failOnNth {
			return errors.New("inject: nth Write16")
		}
	}
	return n.tapTransport.Write16(bar, off, v)
}

// notifyFailRx wraps rxFakeDevice and returns an error from Write16 on
// the targeted notify offset once `armed` is set true. Used to cover
// ReceiveFrame's swallowed-NotifyQueue branch.
type notifyFailRx struct {
	*rxFakeDevice
	failOff uint64
	armed   bool
}

func (n *notifyFailRx) Write32(bar uint8, off uint64, v uint32) error {
	if n.armed && bar == 0 && off == n.failOff {
		n.armed = false
		return errors.New("inject: notify fail")
	}
	return n.rxFakeDevice.Write32(bar, off, v)
}

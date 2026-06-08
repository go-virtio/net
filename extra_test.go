// Additional tests to push net coverage past 80%.

package net

import (
	"encoding/binary"
	"errors"
	"sync"
	"testing"

	"github.com/go-virtio/common"
)

// rxFakeDevice is like fakeDevice but proactively simulates the
// device-side RX publish: any time the driver re-notifies the RX
// queue, a new used-ring entry is published (with a tiny canned
// frame). This drives the ReceiveFrame happy path.
type rxFakeDevice struct {
	*fakeDevice
	rxMu        sync.Mutex
	rxFramesNeeded int
}

func (d *rxFakeDevice) Write16(bar uint8, off uint64, v uint16) error {
	if err := d.fakeDevice.Write16(bar, off, v); err != nil {
		return err
	}
	// Detect notify writes on the RX queue (notify range
	// 0x1000..0x1FFF with multiplier=4 stride; RX is at offset
	// 0x1000+0*4=0x1000, TX at 0x1000+1*4=0x1004).
	if off == 0x1000 && bar == 0 {
		d.publishRx(v)
	}
	return nil
}

func (d *rxFakeDevice) Write32(bar uint8, off uint64, v uint32) error {
	if err := d.fakeDevice.Write32(bar, off, v); err != nil {
		return err
	}
	if off == 0x1000 && bar == 0 {
		d.publishRx(uint16(v))
	}
	return nil
}

func (d *rxFakeDevice) publishRx(qIdx uint16) {
	d.rxMu.Lock()
	defer d.rxMu.Unlock()
	if d.rxFramesNeeded == 0 {
		return
	}
	d.rxFramesNeeded--
	d.fakeDevice.mu.Lock()
	availAddr := d.fakeDevice.qdriver[qIdx]
	usedAddr := d.fakeDevice.qdevice[qIdx]
	qsize := d.fakeDevice.qsize[qIdx]
	d.fakeDevice.mu.Unlock()
	if availAddr == 0 || usedAddr == 0 {
		return
	}
	// Read avail.idx + ring[0].
	availSlice := readBufferBytes(uintptr(availAddr), 4+int(qsize)*2)
	if availSlice == nil {
		return
	}
	availIdx := binary.LittleEndian.Uint16(availSlice[2:4])
	if availIdx == 0 {
		return
	}
	// Pick the latest published descriptor (avail.idx-1 % size).
	lastSlot := (availIdx - 1) % qsize
	descIdx := binary.LittleEndian.Uint16(availSlice[4+lastSlot*2 : 4+lastSlot*2+2])

	// Read used header + figure out next used slot.
	usedSlice := readBufferBytes(uintptr(usedAddr), 4+int(qsize)*8)
	if usedSlice == nil {
		return
	}
	usedIdx := binary.LittleEndian.Uint16(usedSlice[2:4])
	slot := usedIdx % qsize
	off := 4 + slot*8
	binary.LittleEndian.PutUint32(usedSlice[off:off+4], uint32(descIdx))
	// Publish 16 bytes "frame" length: virtio_net_hdr (12) + a tiny
	// payload (4 bytes of pattern).
	binary.LittleEndian.PutUint32(usedSlice[off+4:off+8], uint32(VirtioNetHdrSize+4))
	binary.LittleEndian.PutUint16(usedSlice[2:4], usedIdx+1)
}

func TestReceiveFrame_RoundTrip(t *testing.T) {
	mac := [6]byte{1, 2, 3, 4, 5, 6}
	d := &rxFakeDevice{
		fakeDevice:     newFakeDevice(FeatureMAC|FeatureMTU|FeatureStatus|common.FeatureVersion1, mac),
		rxFramesNeeded: 1,
	}
	// OpenVirtioNet calls fillRxRing then NotifyQueue on rxq. That
	// notify triggers publishRx, planting one frame.
	v, err := OpenVirtioNet(d)
	if err != nil {
		t.Fatalf("OpenVirtioNet: %v", err)
	}
	frame, err := v.ReceiveFrame(10)
	if err != nil {
		t.Fatalf("ReceiveFrame: %v", err)
	}
	if len(frame) != 4 {
		t.Errorf("RX payload length: got %d, want 4 (header stripped)", len(frame))
	}
}

func TestOpenVirtioNet_FillRxRingAllocFail(t *testing.T) {
	mac := [6]byte{1, 2, 3, 4, 5, 6}
	d := newFakeDevice(FeatureMAC|FeatureMTU|FeatureStatus|common.FeatureVersion1, mac)

	// Make the allocator fail AFTER the virtqueues are allocated.
	// Trick: route through a wrapper that counts to N before failing.
	wrap := &countingFailAlloc{fakeDevice: d, failAfter: 2} // 2 virtqueues, then RX-buf fills fail
	if _, err := OpenVirtioNet(wrap); err == nil {
		t.Errorf("expected fill-rx-ring alloc error")
	}
}

type countingFailAlloc struct {
	*fakeDevice
	calls     int
	failAfter int
}

func (c *countingFailAlloc) AllocatePages(count int) (uint64, []byte, error) {
	c.calls++
	if c.calls > c.failAfter {
		return 0, nil, errors.New("inject: rx ring alloc fail")
	}
	return c.fakeDevice.AllocatePages(count)
}

// allocReturnsZero exposes the ErrAllocReturnedZero branch in
// fillRxRing.
type allocReturnsZero struct{ *fakeDevice }

func (z allocReturnsZero) AllocatePages(count int) (uint64, []byte, error) {
	z.fakeDevice.allocCalls++
	if z.fakeDevice.allocCalls > 2 {
		// First two calls (RX + TX virtqueues) — succeed via parent.
		// After that (fillRxRing), return zero phys.
		mem := make([]byte, count*int(common.PageSize))
		z.fakeDevice.heldPages = append(z.fakeDevice.heldPages, mem)
		return 0, mem, nil
	}
	mem := make([]byte, count*int(common.PageSize))
	z.fakeDevice.heldPages = append(z.fakeDevice.heldPages, mem)
	return uint64(uintptrFromSlice(mem)), mem, nil
}

func TestOpenVirtioNet_FillRxAllocReturnsZero(t *testing.T) {
	mac := [6]byte{1, 2, 3, 4, 5, 6}
	d := newFakeDevice(FeatureMAC|FeatureMTU|FeatureStatus|common.FeatureVersion1, mac)
	wrap := allocReturnsZero{fakeDevice: d}
	if _, err := OpenVirtioNet(wrap); !errors.Is(err, common.ErrAllocReturnedZero) {
		t.Errorf("got %v, want ErrAllocReturnedZero", err)
	}
}

// TestReadBufferBytes_EdgeCases — nil + zero-length returns nil.
func TestReadBufferBytes_EdgeCases(t *testing.T) {
	if got := readBufferBytes(0, 100); got != nil {
		t.Errorf("addr=0 should return nil, got len=%d", len(got))
	}
	if got := readBufferBytes(0xDEADBEEF, 0); got != nil {
		t.Errorf("length=0 should return nil")
	}
}

// TestTransmitFrame_AllocFail covers TransmitFrame's first error path.
func TestTransmitFrame_AllocFail(t *testing.T) {
	mac := [6]byte{1, 2, 3, 4, 5, 6}
	d := newFakeDevice(FeatureMAC|FeatureMTU|FeatureStatus|common.FeatureVersion1, mac)
	v, err := OpenVirtioNet(d)
	if err != nil {
		t.Fatalf("OpenVirtioNet: %v", err)
	}
	// Now make the allocator fail.
	d.allocFail = true
	if err := v.TransmitFrame([]byte{1, 2, 3}); err == nil {
		t.Errorf("expected alloc error")
	}
}

// TestTransmitFrame_AllocReturnsZero covers the phys==0 branch in TX.
func TestTransmitFrame_AllocReturnsZero(t *testing.T) {
	mac := [6]byte{1, 2, 3, 4, 5, 6}
	d := newFakeDevice(FeatureMAC|FeatureMTU|FeatureStatus|common.FeatureVersion1, mac)
	v, err := OpenVirtioNet(d)
	if err != nil {
		t.Fatalf("OpenVirtioNet: %v", err)
	}
	// Switch allocator to return phys=0.
	d.heldPages = append(d.heldPages, make([]byte, 4096))
	// Patch via a wrapper.
	wrap := &zeroPhysWrap{VirtioNet: v, fakeDevice: d}
	if err := wrap.transmitWithZero([]byte{1, 2, 3}); !errors.Is(err, common.ErrAllocReturnedZero) {
		t.Errorf("got %v, want ErrAllocReturnedZero", err)
	}
}

type zeroPhysWrap struct {
	*VirtioNet
	*fakeDevice
}

// transmitWithZero calls TransmitFrame after swapping in an allocator
// that returns 0 for phys.
func (z *zeroPhysWrap) transmitWithZero(frame []byte) error {
	// Save + restore.
	orig := z.VirtioNet.transport
	defer func() { z.VirtioNet.transport = orig }()
	z.VirtioNet.transport = zeroPhysTransport{base: z.fakeDevice}
	return z.VirtioNet.TransmitFrame(frame)
}

type zeroPhysTransport struct{ base *fakeDevice }

func (z zeroPhysTransport) ReadConfig8(o uint8) (uint8, error)   { return z.base.ReadConfig8(o) }
func (z zeroPhysTransport) ReadConfig16(o uint8) (uint16, error) { return z.base.ReadConfig16(o) }
func (z zeroPhysTransport) ReadConfig32(o uint8) (uint32, error) { return z.base.ReadConfig32(o) }
func (z zeroPhysTransport) Read8(b uint8, o uint64) (uint8, error) { return z.base.Read8(b, o) }
func (z zeroPhysTransport) Read16(b uint8, o uint64) (uint16, error) {
	return z.base.Read16(b, o)
}
func (z zeroPhysTransport) Read32(b uint8, o uint64) (uint32, error) {
	return z.base.Read32(b, o)
}
func (z zeroPhysTransport) Read64(b uint8, o uint64) (uint64, error) {
	return z.base.Read64(b, o)
}
func (z zeroPhysTransport) Write8(b uint8, o uint64, v uint8) error {
	return z.base.Write8(b, o, v)
}
func (z zeroPhysTransport) Write16(b uint8, o uint64, v uint16) error {
	return z.base.Write16(b, o, v)
}
func (z zeroPhysTransport) Write32(b uint8, o uint64, v uint32) error {
	return z.base.Write32(b, o, v)
}
func (z zeroPhysTransport) Write64(b uint8, o uint64, v uint64) error {
	return z.base.Write64(b, o, v)
}
func (z zeroPhysTransport) AllocatePages(c int) (uint64, []byte, error) {
	return 0, make([]byte, c*int(common.PageSize)), nil
}

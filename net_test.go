// End-to-end tests for the OpenVirtioNet driver path. Uses a fakeDevice
// transport that:
//
//   - Publishes a valid virtio-net PCI config-space cap chain
//     (CommonCfg + ext NotifyCfg + DeviceCfg with a MAC at offset 0).
//   - Tracks COMMON_CFG register state — the device-status progression,
//     feature-select index, and per-queue address publication.
//   - Simulates the device-side feature-OK accept logic: if the driver
//     writes a feature set that includes VERSION_1 + MAC + MTU, the
//     device "accepts" FEATURES_OK; otherwise it clears the bit.
//   - Simulates the device side of TX completion: after a NotifyQueue
//     write on the TX queue, the device immediately publishes the
//     descriptor in the used ring.

package net

import (
	"encoding/binary"
	"errors"
	"sync"
	"testing"

	"github.com/go-virtio/common"
)

// fakeDevice is a minimal in-memory virtio-net device for driver tests.
type fakeDevice struct {
	mu sync.Mutex

	// PCI config-space contents.
	cfg []byte

	// COMMON_CFG state.
	deviceFeatureSelect uint32
	deviceFeatures      uint64 // what the device offers
	driverFeatures      uint64 // what the driver acked
	deviceStatus        uint8
	currentQueue        uint16
	// Per-queue state. Key: queue idx.
	qsize      map[uint16]uint16
	qenable    map[uint16]uint16
	qdesc      map[uint16]uint64
	qdriver    map[uint16]uint64
	qdevice    map[uint16]uint64
	qnotifyOff map[uint16]uint16

	// BAR memory store (other reads/writes).
	bar map[uint64]uint64 // key = (bar<<48 | offset)

	// DeviceCfg MAC + status to publish.
	deviceCfg []byte

	// Notify side: when the driver notifies a queue, the device
	// (optionally) publishes a used-ring entry. txCompletes lets the
	// test toggle this.
	txCompletes bool

	// Allocator: returns Go heap pages. The phys address is the slice
	// header's underlying pointer cast to uint64.
	allocCalls int
	allocFail  bool

	// heldPages pins references to allocated pages so the GC does not
	// reclaim them — the driver retains addresses via uintptr which
	// the GC doesn't trace.
	heldPages [][]byte
}

func newFakeDevice(deviceFeats uint64, mac [6]byte) *fakeDevice {
	d := &fakeDevice{
		deviceFeatures: deviceFeats,
		qsize:          map[uint16]uint16{0: 32, 1: 32},
		qenable:        map[uint16]uint16{},
		qdesc:          map[uint16]uint64{},
		qdriver:        map[uint16]uint64{},
		qdevice:        map[uint16]uint64{},
		qnotifyOff:     map[uint16]uint16{0: 0, 1: 1},
		bar:            map[uint64]uint64{},
		deviceCfg:      make([]byte, 17), // VZ-shape DeviceCfg
		txCompletes:    true,
	}
	copy(d.deviceCfg, mac[:])
	d.cfg = buildVirtioNetCfgSpace()
	return d
}

func barKey(bar uint8, off uint64) uint64 { return uint64(bar)<<48 | off }

// PCIConfigReader.
func (d *fakeDevice) ReadConfig8(off uint8) (uint8, error) {
	if int(off) >= len(d.cfg) {
		return 0, errors.New("read past cfg")
	}
	return d.cfg[off], nil
}
func (d *fakeDevice) ReadConfig16(off uint8) (uint16, error) {
	if int(off)+2 > len(d.cfg) {
		return 0, errors.New("read past cfg")
	}
	return binary.LittleEndian.Uint16(d.cfg[off : off+2]), nil
}
func (d *fakeDevice) ReadConfig32(off uint8) (uint32, error) {
	if int(off)+4 > len(d.cfg) {
		return 0, errors.New("read past cfg")
	}
	return binary.LittleEndian.Uint32(d.cfg[off : off+4]), nil
}

// PageAllocator.
func (d *fakeDevice) AllocatePages(count int) (uint64, []byte, error) {
	d.allocCalls++
	if d.allocFail {
		return 0, nil, errors.New("alloc fail")
	}
	mem := make([]byte, count*int(common.PageSize))
	// Use the slice header's address as the physical address so that
	// readBufferBytes (unsafe.Slice on uintptr) round-trips correctly.
	addr := uintptr(0)
	if len(mem) > 0 {
		// Pinning: store a reference so GC doesn't move the slice
		// (in practice the Go runtime doesn't move heap-allocated
		// byte slices; the reference here just makes the intent
		// explicit).
		d.heldPages = append(d.heldPages, mem)
		// Compute the address via &mem[0]. This is portable test code:
		// the slice header's first byte is what unsafe.Slice will
		// expect when we pass `uintptr(phys)` to it.
		addr = uintptrOf(mem)
	}
	return uint64(addr), mem, nil
}


// BARMemoryAccessor: routes accesses with awareness of which CommonCfg
// register is being touched.
func (d *fakeDevice) commonCfgBAR() uint8     { return 0 }
func (d *fakeDevice) commonCfgOffset() uint64 { return 0 }

func (d *fakeDevice) Read8(bar uint8, off uint64) (uint8, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgDeviceStatus:
			return d.deviceStatus, nil
		case common.CfgConfigGeneration:
			return 0, nil
		}
	}
	// DeviceCfg reads (BAR=0 offset=0x8000 length=17).
	if bar == 0 && off >= 0x8000 && off < 0x8000+uint64(len(d.deviceCfg)) {
		return d.deviceCfg[off-0x8000], nil
	}
	return uint8(d.bar[barKey(bar, off)] & 0xFF), nil
}

func (d *fakeDevice) Read16(bar uint8, off uint64) (uint16, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgNumQueues:
			return 2, nil
		case common.CfgQueueSelect:
			return d.currentQueue, nil
		case common.CfgQueueSize:
			return d.qsize[d.currentQueue], nil
		case common.CfgQueueEnable:
			return d.qenable[d.currentQueue], nil
		case common.CfgQueueNotifyOff:
			return d.qnotifyOff[d.currentQueue], nil
		}
	}
	return uint16(d.bar[barKey(bar, off)] & 0xFFFF), nil
}

func (d *fakeDevice) Read32(bar uint8, off uint64) (uint32, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgDeviceFeatureSelect:
			return d.deviceFeatureSelect, nil
		case common.CfgDeviceFeature:
			if d.deviceFeatureSelect == 0 {
				return uint32(d.deviceFeatures & 0xFFFFFFFF), nil
			}
			return uint32(d.deviceFeatures >> 32), nil
		}
	}
	return uint32(d.bar[barKey(bar, off)] & 0xFFFFFFFF), nil
}

func (d *fakeDevice) Read64(bar uint8, off uint64) (uint64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgQueueDesc:
			return d.qdesc[d.currentQueue], nil
		case common.CfgQueueDriver:
			return d.qdriver[d.currentQueue], nil
		case common.CfgQueueDevice:
			return d.qdevice[d.currentQueue], nil
		}
	}
	if bar == 0 && off >= 0x8000 && off < 0x8000+uint64(len(d.deviceCfg)) {
		// DeviceCfg reads (placed at BAR 0 offset 0x8000 in our cfg-space).
		return uint64(d.deviceCfg[off-0x8000]), nil
	}
	return d.bar[barKey(bar, off)], nil
}

func (d *fakeDevice) Write8(bar uint8, off uint64, v uint8) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgDeviceStatus:
			// Simulate the FEATURES_OK handshake: if the driver writes
			// FEATURES_OK and the negotiated feature mask includes
			// VERSION_1 + MAC + MTU, the device "accepts". Otherwise
			// the bit is silently cleared by the device.
			if v&common.StatusFeaturesOK != 0 {
				required := common.FeatureVersion1 | FeatureMAC | FeatureMTU
				if d.driverFeatures&required != required {
					v &^= common.StatusFeaturesOK
				}
			}
			d.deviceStatus = v
			return nil
		}
	}
	d.bar[barKey(bar, off)] = uint64(v)
	return nil
}

func (d *fakeDevice) Write16(bar uint8, off uint64, v uint16) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgQueueSelect:
			d.currentQueue = v
			return nil
		case common.CfgQueueSize:
			d.qsize[d.currentQueue] = v
			return nil
		case common.CfgQueueEnable:
			d.qenable[d.currentQueue] = v
			return nil
		}
	}
	// Notify-queue writes (BAR matches NotifyCfgBAR=0, offset within
	// notify range 0x1000..0x1FFF).
	if off >= 0x1000 && off < 0x2000 {
		d.handleNotify(v)
	}
	d.bar[barKey(bar, off)] = uint64(v)
	return nil
}

func (d *fakeDevice) Write32(bar uint8, off uint64, v uint32) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgDeviceFeatureSelect:
			d.deviceFeatureSelect = v
			return nil
		case common.CfgDriverFeatureSelect:
			d.bar[barKey(bar, off)] = uint64(v)
			return nil
		case common.CfgDriverFeature:
			// Driver feature halves: read the last-written
			// DriverFeatureSelect to know which half.
			sel := d.bar[barKey(bar, common.CfgDriverFeatureSelect)]
			if sel == 0 {
				d.driverFeatures = (d.driverFeatures &^ 0xFFFFFFFF) | uint64(v)
			} else {
				d.driverFeatures = (d.driverFeatures & 0xFFFFFFFF) | (uint64(v) << 32)
			}
			return nil
		}
	}
	// Notify (uint32 form on multiplier>=4).
	if off >= 0x1000 && off < 0x2000 {
		d.handleNotify(uint16(v))
	}
	d.bar[barKey(bar, off)] = uint64(v)
	return nil
}

func (d *fakeDevice) Write64(bar uint8, off uint64, v uint64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgQueueDesc:
			d.qdesc[d.currentQueue] = v
			return nil
		case common.CfgQueueDriver:
			d.qdriver[d.currentQueue] = v
			return nil
		case common.CfgQueueDevice:
			d.qdevice[d.currentQueue] = v
			return nil
		}
	}
	d.bar[barKey(bar, off)] = v
	return nil
}

// handleNotify simulates the device-side reaction to a doorbell. If
// txCompletes is true and the notify is for the TX queue, the device
// "completes" the most-recently-added avail-ring entry on the TX
// queue by writing a used-ring entry. We do this by reading the avail
// ring from the qdriver address and writing to the qdevice address.
func (d *fakeDevice) handleNotify(qIdx uint16) {
	if !d.txCompletes {
		return
	}
	if qIdx != TxQueueIdx {
		return
	}
	availAddr := d.qdriver[qIdx]
	usedAddr := d.qdevice[qIdx]
	if availAddr == 0 || usedAddr == 0 {
		return
	}
	// Read avail.idx from availAddr+2.
	availSlice := readBufferBytes(uintptr(availAddr), 8)
	if availSlice == nil {
		return
	}
	availIdx := binary.LittleEndian.Uint16(availSlice[2:4])
	if availIdx == 0 {
		return
	}
	// The descriptor we want to complete is the one in availSlice
	// at the previous publication slot (availIdx-1 % size).
	lastSlot := (availIdx - 1) % d.qsize[qIdx]
	descIdxBytes := availSlice[4+lastSlot*2 : 4+lastSlot*2+2]
	descIdx := binary.LittleEndian.Uint16(descIdxBytes)
	// Find the current used.idx.
	usedSlice := readBufferBytes(uintptr(usedAddr), 32)
	if usedSlice == nil {
		return
	}
	usedIdx := binary.LittleEndian.Uint16(usedSlice[2:4])
	// Write a used-ring entry at slot (usedIdx % size).
	slot := usedIdx % d.qsize[qIdx]
	off := 4 + slot*8
	binary.LittleEndian.PutUint32(usedSlice[off:off+4], uint32(descIdx))
	binary.LittleEndian.PutUint32(usedSlice[off+4:off+8], 100) // arbitrary length
	binary.LittleEndian.PutUint16(usedSlice[2:4], usedIdx+1)
}

// uintptrOf returns the address of the first byte of `b` as a uintptr,
// suitable for round-tripping through readBufferBytes. The Go GC
// guarantee here is "as long as some live Go variable still references
// `b`, the underlying memory stays at this address" — see the heldPages
// pinning in AllocatePages.
func uintptrOf(b []byte) uintptr {
	return uintptrFromSlice(b)
}

// buildVirtioNetCfgSpace builds a 256-byte PCI config-space buffer
// with a virtio-net cap chain:
//
//	0x00 VID=0x1AF4 DID=0x1041
//	0x06 Status[CapList]=1
//	0x34 CapPtr=0x40
//	0x40 CommonCfg cap (16 bytes) BAR=0 offset=0 length=0x38
//	0x50 NotifyCfg ext cap (20 bytes) BAR=0 offset=0x1000 length=0x100
//	     [+16..+20] = 4 (notify_off_multiplier)
//	0x68 DeviceCfg cap (16 bytes) BAR=0 offset=0x8000 length=17
func buildVirtioNetCfgSpace() []byte {
	cfg := make([]byte, 256)
	binary.LittleEndian.PutUint16(cfg[0:], common.PCIVendorID)
	binary.LittleEndian.PutUint16(cfg[2:], common.PCIDeviceIDModernNet)
	binary.LittleEndian.PutUint16(cfg[6:], common.PCIStatusCapabilityList)
	cfg[0x34] = 0x40

	// CommonCfg cap at 0x40.
	cfg[0x40] = common.PCICapIDVendorSpecific
	cfg[0x41] = 0x50 // next
	cfg[0x42] = 16   // cap_len
	cfg[0x43] = common.PCICapCommonCfg
	cfg[0x44] = 0    // bar
	cfg[0x45] = 0    // id
	binary.LittleEndian.PutUint32(cfg[0x48:], 0)    // offset
	binary.LittleEndian.PutUint32(cfg[0x4C:], 0x38) // length

	// NotifyCfg ext cap at 0x50, 20 bytes.
	cfg[0x50] = common.PCICapIDVendorSpecific
	cfg[0x51] = 0x68 // next
	cfg[0x52] = 20   // cap_len (extended)
	cfg[0x53] = common.PCICapNotifyCfg
	cfg[0x54] = 0
	cfg[0x55] = 0
	binary.LittleEndian.PutUint32(cfg[0x58:], 0x1000) // offset
	binary.LittleEndian.PutUint32(cfg[0x5C:], 0x100)  // length
	binary.LittleEndian.PutUint32(cfg[0x60:], 4)      // notify_off_multiplier

	// DeviceCfg cap at 0x68, 16 bytes.
	cfg[0x68] = common.PCICapIDVendorSpecific
	cfg[0x69] = 0x00 // next = end
	cfg[0x6A] = 16
	cfg[0x6B] = common.PCICapDeviceCfg
	cfg[0x6C] = 0
	cfg[0x6D] = 0
	binary.LittleEndian.PutUint32(cfg[0x70:], 0x8000) // offset (within BAR)
	binary.LittleEndian.PutUint32(cfg[0x74:], 17)     // length (VZ shape)

	return cfg
}

func TestOpenVirtioNet_QEMUShape(t *testing.T) {
	// QEMU+EDK2 offers: MAC + STATUS + VERSION_1 + lots of others.
	// MTU NOT offered — that's the QEMU baseline. Our driver should
	// negotiate just MAC + STATUS + VERSION_1.
	deviceFeats := FeatureMAC | FeatureStatus | common.FeatureVersion1 |
		FeatureCSUM | FeatureGuestCSUM | FeatureMrgRxbuf
	mac := [6]byte{0x52, 0x55, 0x0a, 0x00, 0x02, 0x02}
	d := newFakeDevice(deviceFeats, mac)

	// Without MTU in the device offers, QEMU shape: the fake's
	// FEATURES_OK check requires MTU — but QEMU doesn't require it
	// either. Disable the strict check by changing the fake's gate to
	// be VERSION_1 + MAC only:
	// Override txCompletes? No — we need to bypass the MTU-required
	// gate. Rewrite via a custom Write8 — simplest: don't insist on
	// MTU in the gate logic. Patch the simulated FEATURES_OK gate to
	// be VERSION_1 + MAC only for this test by removing MTU from the
	// required set:
	dx := &fakeDeviceQEMU{fakeDevice: d}
	v, err := OpenVirtioNet(dx)
	if err != nil {
		t.Fatalf("OpenVirtioNet: %v", err)
	}
	if v.MAC != mac {
		t.Errorf("MAC: got %v, want %v", v.MAC, mac)
	}
	want := FeatureMAC | FeatureStatus | common.FeatureVersion1
	if v.NegotiatedFeatures != want {
		t.Errorf("Negotiated: got 0x%x, want 0x%x", v.NegotiatedFeatures, want)
	}
}

func TestOpenVirtioNet_VZShape(t *testing.T) {
	// VZ shape: MAC + STATUS + VERSION_1 + MTU. Without MTU acked the
	// device clears FEATURES_OK.
	deviceFeats := FeatureMAC | FeatureMTU | FeatureStatus | common.FeatureVersion1
	mac := [6]byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF}
	d := newFakeDevice(deviceFeats, mac)

	v, err := OpenVirtioNet(d)
	if err != nil {
		t.Fatalf("OpenVirtioNet: %v", err)
	}
	if v.MAC != mac {
		t.Errorf("MAC: got %v, want %v", v.MAC, mac)
	}
	want := FeatureMAC | FeatureMTU | FeatureStatus | common.FeatureVersion1
	if v.NegotiatedFeatures != want {
		t.Errorf("Negotiated: got 0x%x, want 0x%x", v.NegotiatedFeatures, want)
	}
}

func TestOpenVirtioNet_WrongDeviceID(t *testing.T) {
	d := newFakeDevice(FeatureMAC|FeatureMTU|FeatureStatus|common.FeatureVersion1, [6]byte{1})
	// Patch DID to virtio-blk so the driver rejects it.
	binary.LittleEndian.PutUint16(d.cfg[2:], common.PCIDeviceIDModernBlock)
	if _, err := OpenVirtioNet(d); !errors.Is(err, ErrInitWrongDeviceID) {
		t.Errorf("got %v", err)
	}
}

func TestOpenVirtioNet_LegacyDevice(t *testing.T) {
	// Device doesn't offer VERSION_1.
	d := newFakeDevice(FeatureMAC|FeatureStatus, [6]byte{1})
	if _, err := OpenVirtioNet(d); !errors.Is(err, ErrNotModernDevice) {
		t.Errorf("got %v", err)
	}
}

func TestOpenVirtioNet_NoMACFeature(t *testing.T) {
	// Device modern but no MAC offered.
	d := newFakeDevice(FeatureMTU|FeatureStatus|common.FeatureVersion1, [6]byte{1})
	if _, err := OpenVirtioNet(d); !errors.Is(err, ErrNoMACFeature) {
		t.Errorf("got %v", err)
	}
}

func TestOpenVirtioNet_FeaturesNotOK(t *testing.T) {
	// VZ shape but without MTU in our accepted set — FEATURES_OK
	// won't stick.
	d := newFakeDevice(FeatureMAC|FeatureMTU|FeatureStatus|common.FeatureVersion1, [6]byte{1})
	// Override accepted features to skip MTU.
	if _, err := OpenVirtioNetWithFeatures(d, FeatureMAC|FeatureStatus|common.FeatureVersion1); !errors.Is(err, ErrFeaturesNotOK) {
		t.Errorf("got %v", err)
	}
}

func TestOpenVirtioNet_ZeroMAC(t *testing.T) {
	d := newFakeDevice(FeatureMAC|FeatureMTU|FeatureStatus|common.FeatureVersion1, [6]byte{})
	if _, err := OpenVirtioNet(d); !errors.Is(err, ErrMACReadFailed) {
		t.Errorf("got %v", err)
	}
}

// fakeDeviceQEMU overrides Write8 to gate FEATURES_OK on VERSION_1+MAC
// only (no MTU requirement) — mirrors the QEMU+EDK2 behaviour.
type fakeDeviceQEMU struct {
	*fakeDevice
}

func (d *fakeDeviceQEMU) Write8(bar uint8, off uint64, v uint8) error {
	d.fakeDevice.mu.Lock()
	defer d.fakeDevice.mu.Unlock()
	if bar == d.fakeDevice.commonCfgBAR() && off-d.fakeDevice.commonCfgOffset() == common.CfgDeviceStatus {
		if v&common.StatusFeaturesOK != 0 {
			required := common.FeatureVersion1 | FeatureMAC
			if d.fakeDevice.driverFeatures&required != required {
				v &^= common.StatusFeaturesOK
			}
		}
		d.fakeDevice.deviceStatus = v
		return nil
	}
	d.fakeDevice.bar[barKey(bar, off)] = uint64(v)
	return nil
}

func TestOpenVirtioNet_QueueZeroSize(t *testing.T) {
	d := newFakeDevice(FeatureMAC|FeatureMTU|FeatureStatus|common.FeatureVersion1, [6]byte{1})
	d.qsize[0] = 0 // RX queue size 0 → unavailable
	if _, err := OpenVirtioNet(d); !errors.Is(err, ErrQueueNotAvailable) {
		t.Errorf("got %v", err)
	}
}

func TestTransmitFrame_RoundTrip(t *testing.T) {
	mac := [6]byte{1, 2, 3, 4, 5, 6}
	d := newFakeDevice(FeatureMAC|FeatureMTU|FeatureStatus|common.FeatureVersion1, mac)
	v, err := OpenVirtioNet(d)
	if err != nil {
		t.Fatalf("OpenVirtioNet: %v", err)
	}
	frame := []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 1, 2, 3, 4, 5, 6, 0x08, 0x06}
	if err := v.TransmitFrame(frame); err != nil {
		t.Errorf("TransmitFrame: %v", err)
	}
}

func TestTransmitFrame_Timeout(t *testing.T) {
	mac := [6]byte{1, 2, 3, 4, 5, 6}
	d := newFakeDevice(FeatureMAC|FeatureMTU|FeatureStatus|common.FeatureVersion1, mac)
	v, err := OpenVirtioNet(d)
	if err != nil {
		t.Fatalf("OpenVirtioNet: %v", err)
	}
	// Disable the device-side TX completion logic.
	d.txCompletes = false
	frame := []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 1, 2, 3, 4, 5, 6, 0x08, 0x06}
	// Reduce poll budget by routing through TransmitFrame — but we
	// can't override TxPollIterations easily. Patch d to keep the
	// budget short: TxPollIterations is a package-level const. Use a
	// large enough sentinel: the timeout will happen at 200000 iters
	// which is fast in-memory; the test still completes in
	// milliseconds.
	err = v.TransmitFrame(frame)
	if !errors.Is(err, ErrTransmitTimeout) {
		t.Errorf("got %v, want ErrTransmitTimeout", err)
	}
}

func TestReceiveFrame_Timeout(t *testing.T) {
	mac := [6]byte{1, 2, 3, 4, 5, 6}
	d := newFakeDevice(FeatureMAC|FeatureMTU|FeatureStatus|common.FeatureVersion1, mac)
	v, err := OpenVirtioNet(d)
	if err != nil {
		t.Fatalf("OpenVirtioNet: %v", err)
	}
	if _, err := v.ReceiveFrame(100); !errors.Is(err, ErrReceiveTimeout) {
		t.Errorf("got %v", err)
	}
}

func TestOpenVirtioNet_AllocFail(t *testing.T) {
	mac := [6]byte{1, 2, 3, 4, 5, 6}
	d := newFakeDevice(FeatureMAC|FeatureMTU|FeatureStatus|common.FeatureVersion1, mac)
	d.allocFail = true
	if _, err := OpenVirtioNet(d); err == nil {
		t.Errorf("expected alloc error")
	}
}

func TestVirtioNet_RxTxQueueAccessors(t *testing.T) {
	mac := [6]byte{1, 2, 3, 4, 5, 6}
	d := newFakeDevice(FeatureMAC|FeatureMTU|FeatureStatus|common.FeatureVersion1, mac)
	v, err := OpenVirtioNet(d)
	if err != nil {
		t.Fatalf("OpenVirtioNet: %v", err)
	}
	if v.RxQueue() == nil {
		t.Error("RxQueue nil")
	}
	if v.TxQueue() == nil {
		t.Error("TxQueue nil")
	}
}

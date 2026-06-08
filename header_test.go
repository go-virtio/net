// Tests for header.go — constants + helpers.

package net

import (
	"bytes"
	"errors"
	"testing"

	"github.com/go-virtio/common"
)

func TestVirtioNetHdrSize(t *testing.T) {
	if VirtioNetHdrSize != 12 {
		t.Errorf("VirtioNetHdrSize: got %d, want 12", VirtioNetHdrSize)
	}
}

func TestPrependVirtioNetHdr(t *testing.T) {
	frame := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	out := PrependVirtioNetHdr(frame)
	if len(out) != VirtioNetHdrSize+len(frame) {
		t.Errorf("len: got %d, want %d", len(out), VirtioNetHdrSize+len(frame))
	}
	for i := 0; i < VirtioNetHdrSize; i++ {
		if out[i] != 0 {
			t.Errorf("header byte %d: 0x%x", i, out[i])
		}
	}
	if !bytes.Equal(out[VirtioNetHdrSize:], frame) {
		t.Errorf("payload: %x", out[VirtioNetHdrSize:])
	}
}

func TestStripVirtioNetHdr(t *testing.T) {
	buf := []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x11, 0x22, 0x33}
	frame, err := StripVirtioNetHdr(buf)
	if err != nil {
		t.Fatalf("Strip: %v", err)
	}
	if !bytes.Equal(frame, []byte{0x11, 0x22, 0x33}) {
		t.Errorf("frame: %x", frame)
	}
}

func TestStripVirtioNetHdr_TooShort(t *testing.T) {
	_, err := StripVirtioNetHdr([]byte{0, 0, 0, 0})
	if !errors.Is(err, ErrFrameTooShort) {
		t.Errorf("got %v", err)
	}
}

func TestQueueIndices(t *testing.T) {
	if RxQueueIdx != 0 {
		t.Errorf("Rx: got %d, want 0", RxQueueIdx)
	}
	if TxQueueIdx != 1 {
		t.Errorf("Tx: got %d, want 1", TxQueueIdx)
	}
}

func TestAcceptedFeatures(t *testing.T) {
	// R-M2b regression guard: MTU bit MUST be included.
	want := FeatureMTU | FeatureMAC | FeatureStatus | common.FeatureVersion1
	if AcceptedFeatures != want {
		t.Errorf("AcceptedFeatures: got 0x%x, want 0x%x", AcceptedFeatures, want)
	}
	if AcceptedFeatures&FeatureMTU == 0 {
		t.Errorf("MTU bit missing — Apple VZ regression")
	}
}

func TestFeatureBits(t *testing.T) {
	cases := []struct {
		name string
		v    uint64
		want uint64
	}{
		{"CSUM", FeatureCSUM, 1 << 0},
		{"GuestCSUM", FeatureGuestCSUM, 1 << 1},
		{"MTU", FeatureMTU, 1 << 3},
		{"MAC", FeatureMAC, 1 << 5},
		{"GuestTSO4", FeatureGuestTSO4, 1 << 7},
		{"GuestTSO6", FeatureGuestTSO6, 1 << 8},
		{"HostTSO4", FeatureHostTSO4, 1 << 11},
		{"HostTSO6", FeatureHostTSO6, 1 << 12},
		{"MrgRxbuf", FeatureMrgRxbuf, 1 << 15},
		{"Status", FeatureStatus, 1 << 16},
		{"MQ", FeatureMQ, 1 << 22},
	}
	for _, c := range cases {
		if c.v != c.want {
			t.Errorf("Feature%s: got 0x%x, want 0x%x", c.name, c.v, c.want)
		}
	}
}

func TestMAC6_String(t *testing.T) {
	m := MAC6{0x52, 0x55, 0x0a, 0x00, 0x02, 0x02}
	got := m.String()
	if got != "52:55:0a:00:02:02" {
		t.Errorf("got %q", got)
	}
}

func TestMAC6_StringEdge(t *testing.T) {
	m := MAC6{0x00, 0xff, 0xab, 0xcd, 0x01, 0xef}
	if got := m.String(); got != "00:ff:ab:cd:01:ef" {
		t.Errorf("got %q", got)
	}
}

func TestMAC6_IsZero(t *testing.T) {
	if !(MAC6{}).IsZero() {
		t.Errorf("zero: IsZero=false")
	}
	if (MAC6{0, 0, 0, 0, 0, 1}).IsZero() {
		t.Errorf("non-zero last: IsZero=true")
	}
	if (MAC6{1, 0, 0, 0, 0, 0}).IsZero() {
		t.Errorf("non-zero first: IsZero=true")
	}
}

func TestAcceptFeatures_HappyPath(t *testing.T) {
	deviceOffers := FeatureMAC | FeatureMTU | FeatureStatus | common.FeatureVersion1 | FeatureMrgRxbuf | FeatureCSUM
	got, err := AcceptFeatures(deviceOffers)
	if err != nil {
		t.Fatalf("AcceptFeatures: %v", err)
	}
	want := FeatureMTU | FeatureMAC | FeatureStatus | common.FeatureVersion1
	if got != want {
		t.Errorf("got 0x%x, want 0x%x", got, want)
	}
}

func TestAcceptFeatures_NoMTU(t *testing.T) {
	// QEMU+EDK2 baseline: MAC + STATUS + VERSION_1 (no MTU). Should
	// succeed; the MTU bit is informational so its absence is not an
	// error.
	deviceOffers := FeatureMAC | FeatureStatus | common.FeatureVersion1
	got, err := AcceptFeatures(deviceOffers)
	if err != nil {
		t.Fatalf("AcceptFeatures: %v", err)
	}
	if got&FeatureMTU != 0 {
		t.Errorf("MTU bit set despite device not offering it")
	}
}

func TestAcceptFeatures_NoVersion1(t *testing.T) {
	deviceOffers := FeatureMAC | FeatureStatus
	if _, err := AcceptFeatures(deviceOffers); !errors.Is(err, ErrNotModernDevice) {
		t.Errorf("got %v", err)
	}
}

func TestAcceptFeatures_NoMAC(t *testing.T) {
	deviceOffers := FeatureStatus | common.FeatureVersion1
	if _, err := AcceptFeatures(deviceOffers); !errors.Is(err, ErrNoMACFeature) {
		t.Errorf("got %v", err)
	}
}

func TestCfgOffsets(t *testing.T) {
	if CfgOffsetMAC != 0 {
		t.Errorf("MAC offset: %d", CfgOffsetMAC)
	}
	if CfgOffsetStatus != 6 {
		t.Errorf("Status offset: %d", CfgOffsetStatus)
	}
	if CfgOffsetMaxVirtqueuePairs != 8 {
		t.Errorf("MaxVQPairs offset: %d", CfgOffsetMaxVirtqueuePairs)
	}
	if CfgOffsetMTU != 10 {
		t.Errorf("MTU offset: %d", CfgOffsetMTU)
	}
}

func TestSizes(t *testing.T) {
	if MaxFrameSize != 1518 {
		t.Errorf("MaxFrameSize: %d", MaxFrameSize)
	}
	if RxRingSize != 16 {
		t.Errorf("RxRingSize: %d", RxRingSize)
	}
	if TxRingSize != 8 {
		t.Errorf("TxRingSize: %d", TxRingSize)
	}
	if MACLen != 6 {
		t.Errorf("MACLen: %d", MACLen)
	}
}

func TestCommonNetError(t *testing.T) {
	if ErrFrameTooShort.Error() == "" {
		t.Error("empty")
	}
}

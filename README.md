# go-virtio/net

Pure-Go virtio-net driver targeting the `go-virtio/common` transport
interfaces. Implements the modern-transport (Virtio 1.0+) init sequence
and split-virtqueue TX/RX path for the standard PCI-bound virtio-net
device (VID 0x1AF4, DID 0x1041).

This package owns the spec-level driver (the per-frame header layout,
the feature-acceptance mask, the init sequence per Virtio 1.1 §3.1.1,
the rxq/txq state machine) and routes every transport-level operation
through `go-virtio/common`'s `Transport` interface. Drop in any
implementation of that interface (UEFI's `EFI_PCI_IO_PROTOCOL`,
bare-metal MMIO, virtio-mmio adapter) and the same driver code drives
the device.

## Quick start

```go
import (
    virtionet "github.com/go-virtio/net"
)

// transport is any value that implements go-virtio/common.Transport.
vn, err := virtionet.OpenVirtioNet(transport)
if err != nil {
    return err
}
if err := vn.TransmitFrame(ethFrame); err != nil {
    return err
}
frame, err := vn.ReceiveFrame(10000) // poll budget
```

## Sibling packages

  - [`github.com/go-virtio/common`](https://github.com/go-virtio/common)
    — transport-agnostic infrastructure (PCI cap walker, modern config
    layout, split-virtqueue impl, transport interfaces).
  - [`github.com/go-virtio/blk`](https://github.com/go-virtio/blk) —
    placeholder for a future pure-Go virtio-blk driver.

## License

BSD-3-Clause. See [LICENSE](LICENSE).

module github.com/go-virtio/net

go 1.22

require github.com/go-virtio/common v0.0.0-00010101000000-000000000000

// Until go-virtio/common publishes its first tagged release, contributors
// clone both repos side-by-side and this replace directive points at the
// sibling working copy. Remove once `go-virtio/common` is tagged and the
// require line is updated to a real semver version.
replace github.com/go-virtio/common => ../common

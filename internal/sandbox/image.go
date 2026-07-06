package sandbox

// DefaultDiskSizeGiB is the virtual ceiling every VM root disk is grown
// to. The image is sparse, so this bounds runaway growth rather than
// charging steady-state cost — a box only ever occupies the blocks it has
// actually written. Build caches live on shared virtio-fs mounts (see
// ToolchainCacheShares), which keeps each rootfs's working set small.
//
// OCI rootfs disks are padded sparse ext4 images sized to this ceiling at
// build time.
const DefaultDiskSizeGiB = 8

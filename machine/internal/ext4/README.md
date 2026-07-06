# internal/ext4

A user-space ext4 filesystem writer: converts a tar stream into a mountable
ext4 disk image with no root privileges, no loop devices, and no e2fsprogs.

Forked from [Microsoft/hcsshim](https://github.com/Microsoft/hcsshim)
`ext4/tar2ext4` + `ext4/internal/compactext4` at v0.14.1 (MIT — see LICENSE).

Why a vendored fork rather than a dependency:

  1. The reusable core is hcsshim's `ext4/internal/compactext4`, and Go
     forbids importing another module's `internal/` package — so it can't be
     a dependency at all, regardless of its API. Only the thin `tar2ext4`
     wrapper is importable, and it exposes none of the knobs below.
  2. hcsshim targets immutable, read-only LCOW layer images; we build a
     *writable* VM root filesystem, which needs behaviour upstream
     deliberately doesn't offer (the first four changes below).
  3. Depending on hcsshim would drag in logrus, pkg/errors, go-winio and
     dm-verity support we never use; the fork is standard-library-only.

Apple's Containerization framework ships the same design in Swift
(`ContainerizationEXT4.Formatter`).

Local changes relative to upstream:

  - `compactext4`: new `Writable` option. Upstream marks every image with
    `RO_COMPAT_READONLY`, which makes the kernel refuse `mount -o rw` —
    correct for LCOW layer VHDs, fatal for a VM root filesystem.
  - `compactext4`: new `TotalDiskSize` option. Upstream sizes the
    filesystem exactly to its content, leaving zero free blocks; this
    option pads the block groups so the guest has writable headroom
    without needing resize2fs/growpart in the image.
  - `compactext4`: the free-space tail is materialized with
    `Truncate` (a sparse hole) instead of writing literal zeros when the
    target supports it, so an 8 GiB filesystem costs only its content in
    physical blocks.
  - `compactext4`: the inode table is sized to the padded disk, not the
    file count. A content-sized table on a `TotalDiskSize`-padded image
    runs out of inodes (ENOSPC) under file-heavy workloads — npm/pnpm
    trees, a kernel build — while gigabytes of blocks sit free; the fork
    matches mkfs.ext4's ~1-inode-per-16-KiB ratio. (This bumped the on-disk
    format version, so older cached rootfs disks rebuild on next boot.)
  - `tar2ext4.go` is trimmed to the conversion path (package `ext4`):
    VHD footers, dm-verity, Windows backslash handling and the
    OCI-whiteout-to-overlayfs translation are dropped. Whiteout entries
    are rejected loudly — callers are expected to feed an already
    flattened tar (see machine/oci). Hardlinks whose target appears later
    in the stream are deferred and retried, since flattened tars emitted
    by go-containerregistry's mutate.Extract walk layers top-down.
  - `internal/memory` dependency replaced by a local constant.
  - `compactext4`: `findPath` (and through it `lookup`/`MakeParents`)
    follows symlinks in path components, kernel-style (40-link bound,
    inline targets only). Upstream walks directory children literally,
    which fails with "path not found" when adding files under
    merged-usr layouts — Debian-family images make `/sbin`, `/bin` and
    `/lib` symlinks into `/usr`, so injecting `/sbin/clawk-init` into
    e.g. `golang:1.25` hit this immediately.

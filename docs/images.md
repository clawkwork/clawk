# Images

Sandboxes boot an OCI image as their root filesystem — set one with
`vm ( image <ref> )`, the `--image` flag, or rely on the built-in `clawk-dev`
default. Any docker-compatible image works (`golang:1.25`, your own, or a
`docker save` tarball). Images are cached and cloned per sandbox, so only the
first boot from an image pays the build cost.

```sh
clawk image list                       # cached rootfs disks
clawk image gc [--dry-run] [--layers]  # reclaim disks no sandbox needs
```

`clawk-dev` (`ghcr.io/clawkwork/clawk-dev`) bundles `go`, `node` + `pnpm`,
`python3` + `uv`, `rustc` + `cargo`, `bun`, `zig`, plus `git`, `gh`, `jq`,
`ripgrep`, `claude`, and `codex`. The rootfs is rebuilt from the image each
boot, so bake system dependencies into the image and use `on up` for per-boot
setup.

A sandbox records the image *reference* it was created with, and the
reference is re-resolved against the registry each time the rootfs is
rebuilt. Two consequences: a mutable tag (`golang:1.25`, `:latest`) that
moves upstream changes the sandbox's rootfs at its next boot, and
resolving requires the registry to be reachable. For reproducible
sandboxes, pin a digest (`image ghcr.io/you/img@sha256:…`) or use a
local `docker save` tarball.

## Guest kernel override

The vz provider direct-boots a guest kernel. By default this is the clawk
guest kernel: a raw `vmlinux` built from Kata Containers' config plus clawk
fragments (9p-over-vsock caches, fscache, sound), published on the
`clawkwork/clawk` releases. For an architecture clawk doesn't publish, it
falls back to the stock Kata Containers static kernel. Override the kernel
per sandbox with a local `vmlinux` path or an http(s) URL:

```sh
clawk --kernel ~/kernels/vmlinux            # one invocation
# or in clawk.mod:
vm ( kernel https://example.com/vmlinux )
```

The main use is supplying a **KVM-enabled** kernel for nested
virtualization: the default kernel (like stock Kata) ships with KVM disabled,
so the guest has no `/dev/kvm`. A KVM-capable kernel (plus an M3-or-newer Mac
on macOS 15+ with `nested` in clawk.mod) is what lets you run Docker/KVM or
firecracker *inside* a sandbox. The override must be a raw `vmlinux` with
virtio-fs/vsock/virtio-blk built in (so it direct-boots like the default).

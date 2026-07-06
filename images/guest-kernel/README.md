# clawk guest kernel (sound-enabled)

The default clawk guest kernel is Kata Containers' static `vmlinux`
(`machine/kernel/kernel.go`). It's built for containers and has **no sound
support**, so even though the host attaches a virtio-snd microphone device by
default (`CLAWK_AUDIO_INPUT`, see `machine/vz/vz_darwin.go`), the guest has
no driver to turn it into an ALSA card — `/dev/snd` never appears and SoX (the
recorder Claude Code's `/voice` falls back to) reports "no audio capture
device". The kernel is monolithic with no `/lib/modules`, so this can't be
fixed at runtime with `modprobe`; the driver has to be compiled in.

This directory builds a kernel that is **identical to Kata's pinned one plus
the four sound options in `snd.fragment`**:

```
CONFIG_SOUND=y
CONFIG_SND=y
CONFIG_SND_PCM=y
CONFIG_SND_VIRTIO=y
```

`build.sh` reuses Kata's own kernel tooling at the pinned release so the base
config (virtio-console/blk/fs, vsock, balloon, PSI, overlayfs …) that clawk's
boot depends on stays byte-for-byte the same — only sound is added — then
resolves with `make olddefconfig` and builds `vmlinux`. Keep the pins in
`build.sh` in sync with `machine/kernel/kernel.go`.

## Build

CI does this on-demand (`.github/workflows/build-guest-kernel.yml`: manual
dispatch, or a push that touches this dir — never an ordinary push), building
each arch on a native runner and publishing `vmlinux-arm64` / `vmlinux-amd64`
to the `guest-kernel-<ver>-snd` release.

Locally (native to the target arch):

```sh
images/guest-kernel/build.sh        # → images/guest-kernel/vmlinux
```

## Use

Until verified, point a single sandbox at it rather than changing the default:

```
# clawk.mod
vm (
    kernel https://github.com/clawkwork/clawk/releases/download/guest-kernel-6.18.15-snd/vmlinux-arm64
)
```

Verify inside that VM:

```sh
arecord -l            # lists a virtio-snd capture card
ls /dev/snd           # controlC0, pcmC0D0c, …
/voice tap            # records instead of erroring
```

## Make it the default (after verification)

Once a sandbox on this kernel boots cleanly **and** voice records, point the
default at it in `machine/kernel/kernel.go` — the `Override`/fetch path already
handles a raw `vmlinux` URL, so the change is wiring the release URL in as the
default source per arch. Don't flip the default before a boot test: a bad guest
kernel breaks every new sandbox.

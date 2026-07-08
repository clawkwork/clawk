#!/usr/bin/env bash
# Build a clawk guest kernel identical to Kata's pinned one but with sound
# (virtio-snd) compiled in, so voice dictation works inside the VM.
#
# Strategy: reuse Kata's own kernel tooling to fetch the exact kernel source
# and generate the exact .config Kata ships (so every virtio/vsock/PSI/overlay
# option clawk's boot relies on stays identical), then append snd.fragment and
# resolve with `make olddefconfig` before building vmlinux. Minimising the diff
# from the known-good config is what keeps the result bootable.
#
# Pins must track machine/kernel/kernel.go (DefaultKataVersion / the kernel
# version embedded in DefaultBinaryPath).
#
# Output: ./vmlinux (override with OUT=). Run natively per arch — building an
# arm64 kernel wants an arm64 host (the CI matrix uses ubuntu-24.04-arm).
set -euo pipefail

# Kata's kernel tooling is Linux-only: it shells out to Linux build utilities
# and maps `uname -m` through an arch table that knows "aarch64" (Linux) but not
# "arm64" (what macOS reports) — which is why running this on a Mac dies deep
# inside Kata with "unsupported architecture: arm64". Building a Linux kernel on
# macOS would also need a cross-toolchain we don't ship. Fail fast with a
# pointer instead.
if [ "$(uname -s)" != "Linux" ]; then
	cat >&2 <<EOF
ERROR: the guest kernel must be built on a Linux host, not $(uname -s).
Ways to build it:
  - GitHub Actions: the build-guest-kernel workflow (manual dispatch, or push a
    change under images/guest-kernel/) builds arm64 on ubuntu-24.04-arm.
  - Or run this on any aarch64 Linux box (a clawk Linux sandbox works).
EOF
	exit 1
fi

KATA_VERSION="${KATA_VERSION:-3.28.0}"
KERNEL_VERSION="${KERNEL_VERSION:-6.18.15}"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FRAGMENTS="${FRAGMENTS:-$here/snd.fragment $here/ninep.fragment}"
WORKDIR="${WORKDIR:-$here/build}"
OUT="${OUT:-$here/vmlinux}"

echo ">> kata $KATA_VERSION, kernel $KERNEL_VERSION, fragments $FRAGMENTS"
mkdir -p "$WORKDIR"

# 1. Kata kernel tooling at the pinned release tag.
if [ ! -d "$WORKDIR/kata" ]; then
	git clone --depth 1 --branch "$KATA_VERSION" \
		https://github.com/kata-containers/kata-containers "$WORKDIR/kata"
fi
cd "$WORKDIR/kata/tools/packaging/kernel"

# 2. Fetch the kernel source and assemble Kata's default .config for this
#    version+arch. build-kernel.sh leaves a `kata-linux-<ver>-<rev>` symlink
#    in the cwd pointing at the prepared source tree.
./build-kernel.sh -v "$KERNEL_VERSION" -d setup
ksrc="$(readlink -f kata-linux-* | head -1)"
test -f "$ksrc/.config" || { echo "no .config in $ksrc after setup" >&2; exit 1; }

# 3. Overlay the sound + 9p-fd options and let the kernel resolve dependencies.
for frag in $FRAGMENTS; do
	cat "$frag" >> "$ksrc/.config"
done
make -C "$ksrc" olddefconfig

# 4. Confirm the options survived (olddefconfig silently drops options whose
#    deps are unmet — catch that here instead of shipping a soundless kernel
#    or one that can't mount the host-served 9p caches).
for opt in CONFIG_SND_VIRTIO CONFIG_SND CONFIG_SOUND CONFIG_NET_9P_FD CONFIG_9P_FSCACHE; do
	grep -q "^${opt}=y" "$ksrc/.config" || {
		echo "ERROR: ${opt}=y missing after olddefconfig — dependency unmet" >&2
		exit 1
	}
done

# 5. Build the bootable kernel image and collect it into OUT. The format the
#    bootloaders accept differs by arch, so `make vmlinux` (the ELF from the
#    build tree) is NOT the right artifact for arm64:
#      - arm64: Apple's VZLinuxBootLoader (and firecracker-aarch64) boot the raw
#        `arch/arm64/boot/Image` (the flat image with the ARM\x64 boot header).
#        Handing them the ELF vmlinux fails at start with VZError Code=1. `make
#        Image` is `objcopy -O binary -S` of vmlinux — it also drops the ~300MB
#        of DWARF, so the result is ~15MB, matching Kata's shipped kernel.
#      - amd64: firecracker/x86 boots an uncompressed ELF vmlinux; strip its
#        debug info (~300MB) so the asset is the ~15MB loadable image.
case "$(uname -m)" in
	aarch64 | arm64)
		make -C "$ksrc" -j"$(nproc)" Image
		cp "$ksrc/arch/arm64/boot/Image" "$OUT"
		;;
	x86_64 | amd64)
		make -C "$ksrc" -j"$(nproc)" vmlinux
		"${STRIP:-strip}" -s -o "$OUT" "$ksrc/vmlinux"
		;;
	*)
		echo "unsupported build arch $(uname -m)" >&2
		exit 1
		;;
esac
echo ">> built $OUT ($(du -h "$OUT" | cut -f1))"

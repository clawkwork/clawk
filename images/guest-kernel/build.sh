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

KATA_VERSION="${KATA_VERSION:-3.28.0}"
KERNEL_VERSION="${KERNEL_VERSION:-6.18.15}"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FRAGMENT="${FRAGMENT:-$here/snd.fragment}"
WORKDIR="${WORKDIR:-$here/build}"
OUT="${OUT:-$here/vmlinux}"

echo ">> kata $KATA_VERSION, kernel $KERNEL_VERSION, fragment $FRAGMENT"
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

# 3. Overlay the sound options and let the kernel resolve dependencies.
cat "$FRAGMENT" >> "$ksrc/.config"
make -C "$ksrc" olddefconfig

# 4. Confirm the options survived (olddefconfig silently drops options whose
#    deps are unmet — catch that here instead of shipping a soundless kernel).
for opt in CONFIG_SND_VIRTIO CONFIG_SND CONFIG_SOUND; do
	grep -q "^${opt}=y" "$ksrc/.config" || {
		echo "ERROR: ${opt}=y missing after olddefconfig — dependency unmet" >&2
		exit 1
	}
done

# 5. Build and collect vmlinux.
make -C "$ksrc" -j"$(nproc)" vmlinux
cp "$ksrc/vmlinux" "$OUT"
echo ">> built $OUT"

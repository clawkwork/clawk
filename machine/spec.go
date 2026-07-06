package machine

import (
	"fmt"
	"net"
)

// Spec describes a VM. It is the input to Backend.New.
//
// Spec is intentionally the intersection of what the supported backends can
// express. Fields that only make sense on one backend live in backend-specific
// option structs, not here.
type Spec struct {
	// ID is a stable identifier used to name on-disk artifacts (sockets,
	// pidfiles, log files). Must be non-empty and filesystem-safe.
	ID string

	VCPU      uint
	MemoryMiB uint64

	// MemoryMaxMiB is the guest-visible memory ceiling. When it exceeds
	// MemoryMiB, a backend that supports ballooning (firecracker's
	// /balloon, virtio-balloon on VZ) configures the balloon to reclaim
	// (MemoryMaxMiB - MemoryMiB) back to the host at boot and deflate on
	// guest pressure. Zero means "same as MemoryMiB" — no ballooning.
	MemoryMaxMiB uint64

	Boot   Boot
	RootFS RootFS

	// Disks are additional block devices. RootFS handles the boot disk.
	Disks []Disk

	// Net lists network interfaces. Backends that do not support every Net
	// variant return an error from Backend.New.
	Net []Net

	// Shares are virtio-fs mounts exposed to the guest.
	Shares []Share

	// VSockCID is the guest's AF_VSOCK context ID. 0 means the backend picks
	// one. Must be >= 3 if set.
	VSockCID uint32

	Serial Serial

	// NestedVirt enables hardware-assisted nested virtualization, letting
	// the guest run its own VMs. Only honored by backends that report
	// Caps.NestedVirt == true; on vz this requires macOS 15+ and an M3 or
	// newer Apple Silicon chip.
	NestedVirt bool
}

// Validate performs common checks that every backend would otherwise repeat.
// Backends may still reject a Spec for backend-specific reasons.
func (s Spec) Validate() error {
	if s.ID == "" {
		return fmt.Errorf("machine: Spec.ID is required")
	}
	if s.VCPU == 0 {
		return fmt.Errorf("machine: Spec.VCPU must be >= 1")
	}
	if s.MemoryMiB < 64 {
		return fmt.Errorf("machine: Spec.MemoryMiB must be >= 64")
	}
	if s.MemoryMaxMiB != 0 && s.MemoryMaxMiB < s.MemoryMiB {
		return fmt.Errorf("machine: Spec.MemoryMaxMiB (%d) must be >= Spec.MemoryMiB (%d)",
			s.MemoryMaxMiB, s.MemoryMiB)
	}
	if s.Boot == nil {
		return fmt.Errorf("machine: Spec.Boot is required")
	}
	if s.RootFS == nil {
		return fmt.Errorf("machine: Spec.RootFS is required")
	}
	if s.VSockCID != 0 && s.VSockCID < 3 {
		return fmt.Errorf("machine: Spec.VSockCID must be 0 or >= 3")
	}
	return nil
}

// Boot describes how the guest kernel is loaded. Sealed union.
type Boot interface{ isBoot() }

// DirectKernel loads an uncompressed vmlinux directly, optionally with an
// initrd, and passes Cmdline on the kernel command line. Supported by every
// backend in the module.
type DirectKernel struct {
	Vmlinux string
	Initrd  string
	Cmdline string
}

func (DirectKernel) isBoot() {}

// EFIBoot uses the platform's EFI firmware to read a kernel from the
// attached rootfs at a standard UEFI path. Use when the rootfs is a stock
// cloud image (Ubuntu, Fedora) that ships a GRUB binary. Firecracker does
// not support this mode; vz does.
type EFIBoot struct {
	// StorePath is the file that backs persistent EFI NVRAM. Created on
	// first use; read on subsequent boots so boot order and other UEFI
	// state survive restarts. Required.
	StorePath string
}

func (EFIBoot) isBoot() {}

// RootFS describes the guest's root filesystem. Sealed union.
type RootFS interface{ isRootFS() }

// OCIImage pulls an OCI image reference and materializes its merged layers as
// an ext4 image. The image digest is the cache key; a second machine using the
// same Ref reuses the built image.
type OCIImage struct {
	// Ref is a fully-qualified OCI image reference
	// (e.g. "ghcr.io/clawkwork/clawk-agent:latest").
	Ref string

	// CacheDir stores digest-keyed built images. Required.
	CacheDir string

	// SizeMiB is the minimum filesystem size in MiB; the gap between the
	// image content and this floor is free space the guest can write into
	// without growpart/resize2fs in the image. Sparse, so a generous floor
	// costs no physical disk. Zero means the builder's default (1 GiB).
	SizeMiB int

	// Platform forces a specific OCI platform ("linux/amd64",
	// "linux/arm64"). Empty picks the registry default for the host arch.
	Platform string

	// Inject is a list of host files written into the built filesystem on
	// top of the image content. Used to plant a guest init or agent into
	// arbitrary images that ship neither. The injected content is part of
	// the disk cache key, so two machines injecting different binaries
	// never share a built disk.
	Inject []InjectFile
}

func (OCIImage) isRootFS() {}

// InjectFile is one host file copied into an OCIImage-built filesystem.
type InjectFile struct {
	// GuestPath is the absolute destination inside the image
	// (e.g. "/sbin/clawk-init"). Parent directories are created as needed.
	GuestPath string

	// HostPath is the file whose content is injected.
	HostPath string

	// Mode is the file mode inside the image (e.g. 0o755).
	Mode uint32
}

// RawDisk boots from an existing block image (raw, ext4, squashfs, etc.).
// The backend decides whether to copy-on-write it or mount it directly.
type RawDisk struct {
	Path     string
	ReadOnly bool
}

func (RawDisk) isRootFS() {}

// Disk is a non-root block device attached to the guest.
type Disk struct {
	Path     string
	ReadOnly bool
}

// Net describes a guest network interface. Sealed union.
type Net interface{ isNet() }

// UserMode runs an in-process userspace TCP/IP stack (gvisor-tap-vsock).
// Works identically on macOS and Linux. No host root required.
//
// Two attachment modes:
//
//   - fd NIC (GuestTAP and HostTAP empty): the backend exposes the stack's
//     unixgram socket as the VM's NIC fd directly. Used by vz, whose
//     file-handle NIC speaks the same datagram protocol gvproxy does.
//
//   - TAP bridge (GuestTAP and HostTAP set): the VM speaks only a host TAP
//     (firecracker). The backend boots the VM's virtio-net on GuestTAP and
//     runs gvproxy bridged to HostTAP — a daemon-owned TAP on the same L2
//     bridge — through a userspace frame pump. Both TAPs must be pre-created
//     by the caller (firecracker can't drive gvproxy's socket transport, and
//     gvproxy can't drive a TAP fd, so the two are joined at L2). The backend
//     assigns the NIC the MAC gvproxy's DHCP lease expects.
type UserMode struct {
	Forwards []PortForward

	// Filter, if non-nil, is consulted on every outbound TCP SYN and every DNS
	// answer. A nil Filter allows everything.
	Filter Filter

	// GuestTAP/HostTAP select the TAP-bridge mode (see above). Linux-only;
	// ignored by backends that attach the fd NIC.
	GuestTAP string
	HostTAP  string
}

func (UserMode) isNet() {}

// TAP attaches to an existing host TAP device. Linux only. The caller is
// responsible for creating the device, assigning host-side IPs, and
// configuring packet forwarding.
type TAP struct {
	Device string
	MAC    string
}

func (TAP) isNet() {}

// Unixgram is a unixgram-socket NIC as used by vz and firecracker. The
// caller brings their own network stack on the other end (typically gvproxy).
type Unixgram struct {
	Path string
	MAC  string
}

func (Unixgram) isNet() {}

// PortForward exposes a guest port on the host. Only valid inside a UserMode.
type PortForward struct {
	HostPort  uint16
	GuestIP   string
	GuestPort uint16
	Proto     Proto
}

// Proto is the L4 protocol for a PortForward.
type Proto string

const (
	ProtoTCP Proto = "tcp"
	ProtoUDP Proto = "udp"
)

// Filter is the policy hook consulted by UserMode networking.
// Implementations must be safe for concurrent use.
//
// The shape matches gvisor-tap-vsock's forwarder and DNS hooks directly so
// bridging is a no-op.
type Filter interface {
	// AllowTCP is called before the userspace TCP stack establishes an
	// outbound connection. addr is "host:port" where host is a literal IP.
	// A non-nil error drops the SYN and is logged by the backend.
	AllowTCP(addr string) error

	// AllowUDP is called before the userspace stack forwards a new outbound
	// UDP flow. addr is "host:port" where host is a literal IP. A non-nil
	// error drops the flow and is logged by the backend.
	AllowUDP(addr string) error

	// AllowICMP is called before the userspace stack forwards an outbound
	// ICMP echo (ping). addr is a literal destination IP (ICMP has no
	// port). A non-nil error drops the packet and is logged by the backend.
	AllowICMP(addr string) error

	// ObserveDNS is called after the userspace DNS responder returns an
	// answer to the guest. Implementations may use this to auto-allow IPs
	// resolved from allow-listed domains (wildcard support).
	ObserveDNS(name string, ip net.IP)
}

// Share is a virtio-fs mount exposed to the guest.
type Share struct {
	// Tag is the mount tag the guest uses to address the share.
	Tag      string
	HostPath string
	ReadOnly bool
}

// Serial configures the guest serial console.
type Serial struct {
	// LogPath, if non-empty, captures serial output. Empty discards it.
	LogPath string
}

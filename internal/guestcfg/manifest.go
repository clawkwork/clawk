// Package guestcfg defines the boot manifest consumed by clawk-init (the
// injected PID 1 of OCI-image sandboxes) and writes it as a raw config
// disk.
//
// The manifest is plain JSON written directly onto a small block device —
// no filesystem, no ISO. clawk-init json-decodes the device and stops at
// the end of the top-level value, so the zero padding that block-aligns
// the file is never read. It is the guest config channel clawk-init
// reads at boot, and works on both vz and firecracker (virtio-fs, which
// a share-based config channel would need, doesn't exist on firecracker).
//
// The schema here is kept in lock-step with the manifest types inlined in
// internal/agentembed/init_main.go.in — same pattern as vsockproto and
// the agent. Bump Version on breaking changes.
package guestcfg

import (
	"encoding/json"
	"fmt"
	"os"
)

// Version is the current manifest schema version. clawk-init refuses
// manifests with a different version, which turns host/guest skew into a
// readable console error instead of silent misbehavior.
//
// Compatibility policy: clawk-init is baked into the sandbox disk at
// create and never updated in place, so additive schema changes (new
// optional fields old inits ignore — JSON decoding is lenient) must NOT
// bump this. A breaking change bumps it together with
// sandbox.CurrentGuestABI, and the host refuses old sandboxes up front
// via sandbox.CheckGuestABI — the in-guest check is the backstop.
const Version = 1

// Guest paths of the injected binaries. The disk builder injects to these
// paths and the manifest's Services reference them.
const (
	InitPath     = "/sbin/clawk-init"
	AgentPath    = "/opt/clawk/bin/clawk-pty-agent"
	TimeSyncPath = "/opt/clawk/bin/clawk-time-sync"
)

// Manifest is everything clawk-init needs to turn a freshly booted OCI
// rootfs into a clawk sandbox.
type Manifest struct {
	Version  int       `json:"version"`
	Hostname string    `json:"hostname,omitempty"`
	Network  *Network  `json:"network,omitempty"`
	User     *User     `json:"user,omitempty"`
	Mounts   []Mount   `json:"mounts,omitempty"`
	Files    []File    `json:"files,omitempty"`
	Services []Service `json:"services,omitempty"`
}

// Network is the static interface configuration. gvproxy assigns a fixed
// DHCP lease to the sandbox MAC, but arbitrary images have no DHCP
// client — clawk-init configures the same values statically instead.
type Network struct {
	Interface string   `json:"interface"`
	Address   string   `json:"address"` // CIDR, e.g. "192.168.127.2/24"
	Gateway   string   `json:"gateway"`
	DNS       []string `json:"dns,omitempty"`
	MTU       int      `json:"mtu,omitempty"`
}

// User is the sandbox user created on first boot. UID/GID mirror the host
// user so virtio-fs share ownership lines up between host and guest.
type User struct {
	Name   string   `json:"name"`
	UID    int      `json:"uid"`
	GID    int      `json:"gid"`
	Groups []string `json:"groups,omitempty"` // joined only if they exist in the image
	Shell  string   `json:"shell,omitempty"`  // empty: /bin/bash if present, else /bin/sh
}

// Mount is one share to mount in the guest.
//
// Transport: a Mount is virtio-fs by default (keyed on Tag). When
// NinePVSockPort is non-zero, a 9p-capable clawk-init instead mounts it over
// 9p2000.L from the host ninep server on that guest vsock port (see
// internal/ninep) — used for the toolchain caches, whose file counts make
// Apple's virtio-fs pin enough host fds to exhaust kern.maxfiles and panic the
// Mac. Tag stays populated as the fallback: an older clawk-init that predates
// 9p support ignores NinePVSockPort (additive field, lenient JSON) and mounts
// the virtio-fs share, and a 9p-capable init falls back to it if the 9p mount
// fails (e.g. a kernel without CONFIG_NET_9P_FD). This keeps the change
// additive — no Version bump, no forced sandbox recreation.
type Mount struct {
	Tag            string `json:"tag"`
	Path           string `json:"path"`
	ReadOnly       bool   `json:"ro,omitempty"`
	NinePVSockPort uint32 `json:"ninep_port,omitempty"`
}

// File is one file written into the guest at boot. Content is raw bytes
// (base64 on the wire via encoding/json).
type File struct {
	Path    string `json:"path"`
	Mode    uint32 `json:"mode"`
	Owner   string `json:"owner,omitempty"` // "user" → the manifest user; default root
	Content []byte `json:"content"`
}

// Service is a long-running process clawk-init starts as root and
// restarts on exit.
type Service struct {
	Name string   `json:"name"`
	Path string   `json:"path"`
	Args []string `json:"args,omitempty"`
}

// sectorSize is the block alignment virtio-blk attachments require of
// their backing file on both vz and firecracker.
const sectorSize = 512

// WriteDisk marshals m and writes it as a raw config disk at path,
// zero-padded to a sector boundary. Written via a temp file + rename so a
// crash never leaves a torn manifest behind a valid-looking path.
func WriteDisk(m Manifest, path string) error {
	if m.Version == 0 {
		m.Version = Version
	}
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("guestcfg: marshalling manifest: %w", err)
	}
	if pad := len(data) % sectorSize; pad != 0 {
		data = append(data, make([]byte, sectorSize-pad)...)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("guestcfg: writing config disk: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("guestcfg: promoting config disk: %w", err)
	}
	return nil
}

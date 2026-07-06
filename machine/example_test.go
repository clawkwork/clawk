package machine_test

import (
	"context"
	"log"

	"github.com/clawkwork/clawk/machine"

	// Side-effect imports register backends. Import only the ones whose
	// runtime you intend to support. Unused imports are free — they register
	// nothing on the wrong OS.
	_ "github.com/clawkwork/clawk/machine/firecracker"
	_ "github.com/clawkwork/clawk/machine/vz"
)

// Example_bootOCI shows the simplest way to boot a VM from an OCI image on
// macOS (vz, Apple Virtualization.framework). The same Spec works on Linux
// with backend "firecracker" — only the Net variant changes (TAP for
// firecracker, UserMode for vz).
func Example_bootOCI() {
	ctx := context.Background()

	b, err := machine.Get("vz")
	if err != nil {
		log.Fatal(err)
	}

	m, err := b.New(ctx, machine.Spec{
		ID:        "demo",
		VCPU:      2,
		MemoryMiB: 1024,
		Boot: machine.DirectKernel{
			Vmlinux: "/etc/vmlinux", // caller supplies a direct-boot kernel
			Cmdline: "console=hvc0 root=/dev/vda rw",
		},
		RootFS: machine.OCIImage{
			Ref:      "docker.io/library/alpine:3.20",
			CacheDir: "/var/cache/clawk/oci",
		},
		Net: []machine.Net{
			machine.UserMode{
				Forwards: []machine.PortForward{
					{HostPort: 2222, GuestPort: 22, Proto: machine.ProtoTCP},
				},
			},
		},
	}, "/var/lib/clawk/vms/demo")
	if err != nil {
		log.Fatal(err)
	}

	if err := m.Create(ctx); err != nil {
		log.Fatal(err)
	}
	if err := m.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer m.Destroy(ctx)
}

// Example_snapshot demonstrates the Snapshottable optional capability.
// Only backends whose Caps.Snapshot is true satisfy this interface.
func Example_snapshot() {
	ctx := context.Background()
	b, err := machine.Get("firecracker")
	if err != nil {
		log.Fatal(err)
	}
	if !b.Capabilities().Snapshot {
		return
	}
	m, err := b.New(ctx, machine.Spec{ /* … */ }, "/tmp/vm")
	if err != nil {
		log.Fatal(err)
	}
	snap, ok := m.(machine.Snapshottable)
	if !ok {
		log.Fatal("backend claims snapshot capability but does not implement Snapshottable")
	}
	if err := snap.Snapshot(ctx, "/tmp/vm/snap"); err != nil {
		log.Fatal(err)
	}
}

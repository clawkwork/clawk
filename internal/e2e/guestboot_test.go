//go:build linux

// Package e2e boot-tests the OCI sandbox guest stack — injected
// clawk-init as PID 1, host-built agent, manifest config disk — on a real
// VM via the firecracker backend. This is the same rootfs + init + agent
// combination the vz provider boots on macOS; only the hypervisor (and
// virtio-fs, which firecracker lacks) differs.
//
// Gated: needs /dev/kvm, the firecracker binary, and network (kernel +
// alpine pull on first run).
//
//	TEST_GUEST_BOOT=1 go test ./internal/e2e -v
//	TEST_GUEST_BOOT_CACHE=~/.cache/clawk-e2e   # optional persistent cache
package e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/clawkwork/clawk/internal/guestbuild"
	"github.com/clawkwork/clawk/internal/guestcfg"
	"github.com/clawkwork/clawk/internal/vsockproto"
	"github.com/clawkwork/clawk/machine"
	"github.com/clawkwork/clawk/machine/kernel"
	"github.com/stretchr/testify/require"

	// Side-effect import: registers "firecracker" with the machine registry.
	_ "github.com/clawkwork/clawk/machine/firecracker"
)

const (
	testImage    = "docker.io/library/alpine:3.20"
	agentPort    = 1024
	bootDeadline = 60 * time.Second
)

func TestGuestBoot(t *testing.T) {
	if os.Getenv("TEST_GUEST_BOOT") == "" {
		t.Skip("set TEST_GUEST_BOOT=1 to run (boots a firecracker VM; needs /dev/kvm + network)")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("/dev/kvm: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cache := os.Getenv("TEST_GUEST_BOOT_CACHE")
	if cache == "" {
		cache = t.TempDir()
	}

	bins, err := guestbuild.Build(ctx, cache, runtime.GOARCH)
	require.NoError(t, err, "guestbuild")
	vmlinux, err := kernel.Fetch(ctx, kernel.Options{CacheDir: cache, Arch: runtime.GOARCH})
	require.NoError(t, err, "kernel")

	stateDir := t.TempDir()
	cfgDisk := filepath.Join(stateDir, "guestcfg.img")
	err = guestcfg.WriteDisk(guestcfg.Manifest{
		Hostname: "clawk-e2e",
		User:     &guestcfg.User{Name: "agent", UID: 1000, GID: 1000},
		Files: []guestcfg.File{{
			Path: "/etc/profile.d/99-clawk-env.sh", Mode: 0o644,
			Content: []byte("export CLAWK_E2E=yes\n"),
		}},
		Services: []guestcfg.Service{
			{Name: "pty-agent", Path: guestcfg.AgentPath},
			{Name: "time-sync", Path: guestcfg.TimeSyncPath},
		},
	}, cfgDisk)
	require.NoError(t, err, "guestcfg")

	spec := machine.Spec{
		ID:        "guestboot-e2e",
		VCPU:      1,
		MemoryMiB: 512,
		Boot: machine.DirectKernel{
			Vmlinux: vmlinux,
			Cmdline: "console=ttyS0 root=/dev/vda rw init=" + guestcfg.InitPath +
				" clawk.cfg=/dev/vdb",
		},
		RootFS: machine.OCIImage{
			Ref:      testImage,
			CacheDir: filepath.Join(cache, "oci"),
			Platform: "linux/" + runtime.GOARCH,
			Inject: []machine.InjectFile{
				{GuestPath: guestcfg.InitPath, HostPath: bins.Init, Mode: 0o755},
				{GuestPath: guestcfg.AgentPath, HostPath: bins.Agent, Mode: 0o755},
				{GuestPath: guestcfg.TimeSyncPath, HostPath: bins.TimeSync, Mode: 0o755},
			},
		},
		Disks:    []machine.Disk{{Path: cfgDisk, ReadOnly: true}},
		VSockCID: 3,
	}

	backend, err := machine.Get("firecracker")
	require.NoError(t, err, "machine.Get")
	m, err := backend.New(ctx, spec, stateDir)
	require.NoError(t, err, "New")
	require.NoError(t, m.Create(ctx), "Create")
	require.NoError(t, m.Start(ctx), "Start")
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = m.Stop(stopCtx, false)
		dumpConsole(t, stateDir)
	})

	conn := dialAgent(t, ctx, m)
	out, exit := runOverAgent(t, conn, vsockproto.Handshake{
		Cmd:  "/bin/sh",
		Args: []string{"-c", "id; cat /etc/hostname; cat /etc/profile.d/99-clawk-env.sh"},
		User: "agent",
	}, "")
	conn.Close()
	if exit != 0 {
		t.Errorf("exit code = %d, want 0; output:\n%s", exit, out)
	}
	for _, want := range []string{"uid=1000(agent)", "clawk-e2e", "CLAWK_E2E=yes"} {
		require.True(t, strings.Contains(out, want), "output missing %q:\n%s", want, out)
	}

	// Bare command name: the agent process is spawned by PID 1 with an
	// empty environment, and exec.LookPath consults the agent's own env
	// — without a PATH fix every non-absolute Cmd fails. This was the
	// "agent disconnected before exit frame: EOF" bug on real sandboxes.
	t.Run("bare command name resolves via PATH", func(t *testing.T) {
		conn := dialAgent(t, ctx, m)
		defer conn.Close()
		out, exit := runOverAgent(t, conn, vsockproto.Handshake{
			Cmd: "id", User: "agent",
		}, "")
		if exit != 0 || !strings.Contains(out, "uid=1000") {
			t.Errorf("exit = %d, output:\n%s", exit, out)
		}
	})

	// Missing command: the agent must answer with a readable error and
	// an exit frame, not close the connection (which clients report as
	// an opaque EOF indistinguishable from a wedged VM).
	t.Run("missing command gets exit frame", func(t *testing.T) {
		conn := dialAgent(t, ctx, m)
		defer conn.Close()
		out, exit := runOverAgent(t, conn, vsockproto.Handshake{
			Cmd: "definitely-not-installed", User: "agent",
		}, "")
		if exit != 127 {
			t.Errorf("exit = %d, want 127; output:\n%s", exit, out)
		}
		require.True(t, strings.Contains(out, "definitely-not-installed"), "error output should name the missing command:\n%s", out)
	})

	// Empty Cmd: the agent must resolve the user's login shell from
	// /etc/passwd — alpine has no bash, so the historical hardcoded
	// /bin/bash default would fail here.
	t.Run("default shell on bashless image", func(t *testing.T) {
		conn := dialAgent(t, ctx, m)
		defer conn.Close()
		out, exit := runOverAgent(t, conn, vsockproto.Handshake{User: "agent"},
			"echo SHELL0=$0; exit\n")
		if exit != 0 {
			t.Errorf("exit = %d; output:\n%s", exit, out)
		}
		require.True(t, strings.Contains(out, "SHELL0=/bin/sh"), "login shell not resolved from passwd:\n%s", out)
	})
}

// TestGuestBootRealImage boots an arbitrary real image (default: the
// Docker claude-code sandbox template) and verifies the parts that only
// real images exercise: uid reconciliation of a baked-in agent user
// (the template ships agent at uid 1000; the manifest mirrors a host
// uid that differs), the image env reaching sessions, and the agent
// CLI being resolvable. Heavier than TestGuestBoot — it pulls the real
// image — so it's gated separately:
//
//	TEST_GUEST_BOOT_IMAGE=docker.io/docker/sandbox-templates:claude-code \
//	  TEST_GUEST_BOOT_CACHE=~/.cache/clawk-e2e go test ./internal/e2e -run RealImage -v
func TestGuestBootRealImage(t *testing.T) {
	image := os.Getenv("TEST_GUEST_BOOT_IMAGE")
	if image == "" {
		t.Skip("set TEST_GUEST_BOOT_IMAGE to run (pulls a real image)")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("/dev/kvm: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	cache := os.Getenv("TEST_GUEST_BOOT_CACHE")
	if cache == "" {
		cache = t.TempDir()
	}

	bins, err := guestbuild.Build(ctx, cache, runtime.GOARCH)
	require.NoError(t, err, "guestbuild")
	vmlinux, err := kernel.Fetch(ctx, kernel.Options{CacheDir: cache, Arch: runtime.GOARCH})
	require.NoError(t, err, "kernel")

	stateDir := t.TempDir()
	cfgDisk := filepath.Join(stateDir, "guestcfg.img")
	// 501 on purpose: differs from the template's baked uid 1000, so the
	// reconcile path must rewrite passwd and re-own the home tree.
	err = guestcfg.WriteDisk(guestcfg.Manifest{
		Hostname: "clawk-real",
		User:     &guestcfg.User{Name: "agent", UID: 501, GID: 20},
		Services: []guestcfg.Service{
			{Name: "pty-agent", Path: guestcfg.AgentPath},
		},
	}, cfgDisk)
	require.NoError(t, err, "guestcfg")

	spec := machine.Spec{
		ID:        "guestboot-real",
		VCPU:      2,
		MemoryMiB: 1024,
		Boot: machine.DirectKernel{
			Vmlinux: vmlinux,
			Cmdline: "console=ttyS0 root=/dev/vda rw init=" + guestcfg.InitPath +
				" clawk.cfg=/dev/vdb",
		},
		RootFS: machine.OCIImage{
			Ref:      image,
			CacheDir: filepath.Join(cache, "oci"),
			Platform: "linux/" + runtime.GOARCH,
			Inject: []machine.InjectFile{
				{GuestPath: guestcfg.InitPath, HostPath: bins.Init, Mode: 0o755},
				{GuestPath: guestcfg.AgentPath, HostPath: bins.Agent, Mode: 0o755},
			},
		},
		Disks:    []machine.Disk{{Path: cfgDisk, ReadOnly: true}},
		VSockCID: 3,
	}

	backend, err := machine.Get("firecracker")
	require.NoError(t, err)
	m, err := backend.New(ctx, spec, stateDir)
	require.NoError(t, err)
	require.NoError(t, m.Create(ctx), "Create (pull+build can take a while on first run)")
	require.NoError(t, m.Start(ctx), "Start")
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = m.Stop(stopCtx, false)
		dumpConsole(t, stateDir)
	})

	conn := dialAgent(t, ctx, m)
	defer conn.Close()
	out, exit := runOverAgent(t, conn, vsockproto.Handshake{
		Cmd:  "/bin/sh",
		Args: []string{"-c", "id; echo HOME=$HOME; ls -ldn $HOME; command -v claude; command -v node"},
		User: "agent",
	}, "")
	if exit != 0 {
		t.Errorf("exit = %d; output:\n%s", exit, out)
	}
	for _, want := range []string{"uid=501", "claude", "node"} {
		require.True(t, strings.Contains(out, want), "output missing %q:\n%s", want, out)
	}
	// Home re-owned to the reconciled uid.
	require.True(t, strings.Contains(out, " 501 "), "home dir not owned by reconciled uid:\n%s", out)
	t.Logf("real image session output:\n%s", out)
}

// dialAgent polls the guest's vsock agent port until the agent accepts —
// this is the boot-readiness signal (kernel booted, init ran, user
// created, agent listening).
func dialAgent(t *testing.T, ctx context.Context, m machine.Machine) net.Conn {
	t.Helper()
	start := time.Now()
	deadline := start.Add(bootDeadline)
	var lastErr error
	for time.Now().Before(deadline) {
		dialCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		conn, err := m.VSock(dialCtx, agentPort)
		cancel()
		if err == nil {
			t.Logf("agent reachable after %s", time.Since(start).Round(time.Millisecond))
			return conn
		}
		lastErr = err
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("agent not reachable within %s: %v", bootDeadline, lastErr)
	return nil
}

// runOverAgent performs one vsockproto session: handshake, optional
// stdin bytes, drain data frames, return the collected output and exit
// code.
func runOverAgent(t *testing.T, conn net.Conn, h vsockproto.Handshake, stdin string) (string, int32) {
	t.Helper()
	payload, err := vsockproto.MarshalHandshake(h)
	require.NoError(t, err, "handshake marshal")
	require.NoError(t, vsockproto.WriteFrame(conn, vsockproto.FrameHandshake, payload), "handshake write")
	if stdin != "" {
		require.NoError(t, vsockproto.WriteFrame(conn, vsockproto.FrameData, []byte(stdin)), "stdin write")
	}
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	var out strings.Builder
	for {
		ft, data, err := vsockproto.ReadFrame(conn)
		if err != nil {
			t.Fatalf("read frame (output so far: %q): %v", out.String(), err)
		}
		switch ft {
		case vsockproto.FrameData:
			out.Write(data)
		case vsockproto.FrameExit:
			code, err := vsockproto.DecodeExit(data)
			require.NoError(t, err, "decode exit")
			return out.String(), code
		}
	}
}

// dumpConsole prints the firecracker log (which carries the guest serial
// console) so a failed boot is diagnosable from the test output.
func dumpConsole(t *testing.T, stateDir string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(stateDir, "firecracker.log"))
	if err != nil {
		t.Logf("no console log: %v", err)
		return
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > 80 {
		lines = lines[len(lines)-80:]
	}
	fmt.Println("--- guest console (tail) ---")
	for _, l := range lines {
		fmt.Println(l)
	}
	fmt.Println("----------------------------")
}

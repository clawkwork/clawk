# clawk architecture

How clawk is put together, for contributors. For *using* clawk see the
[README](README.md); for the reasoning behind the subsystem designs see
[DESIGN.md](DESIGN.md).

clawk is a thin, short-lived CLI in front of a long-lived per-sandbox VM. The
CLI process exits as soon as your command finishes, so the VM can't live in
it — every sandbox is owned by a **detached daemon** the CLI spawns and then
talks to over sockets.

## How a command flows

`clawk up` (or the bare `clawk`):

1. The cobra command resolves the sandbox record and its provider, then calls
   `provider.Start`.
2. `Start` spawns a detached daemon — `__vzd` on macOS, `__fcd` on Linux
   (`setsid`, stdio detached) — that owns the VM for its whole lifetime, and
   waits for the in-guest agent to answer over vsock before returning.
3. The daemon builds a `machine.Spec` (CPUs, memory, the OCI rootfs disk, the
   network, a vsock device, the serial log), asks the `machine` library for
   the right backend, and boots the VM. It also brings up gvproxy and, on
   macOS, the agent proxy and ssh-agent proxy.

`clawk` / `clawk run <runner>` then connect to the **in-guest pty-agent over
vsock** (AF_VSOCK port 1024). Each attach spawns a fresh child in the guest and
tears it down on disconnect — container-exec semantics, not a long-lived TTY.
There is no sshd in either provider.

On macOS the daemon also runs an **idle watchdog**: once the sandbox has had
no attached session and a quiescent guest (load and network counters from the
agent's vsock snapshot, port 1027) for its idle timeout, the daemon records
the park in the sandbox record (`stop_reason: idle`, `DesiredState`
untouched) and stops the VM to hand its memory back to the host. Any attach
or run verb boots it back; `internal/cli/idle.go` holds the decision logic.

## The guest stack

Sandboxes boot an OCI image as an ext4 rootfs with three small Go binaries
injected (`internal/agentembed`, cross-compiled on the host at build time):

- **clawk-init** — PID 1. Sets up `/dev`, networking, the workspace, and the
  guest user, then starts the services named in its config disk.
- **clawk-pty-agent** — listens on AF_VSOCK 1024, allocates a PTY per session,
  and speaks the framing in `internal/vsockproto` to the host
  (`internal/vsockclient`). This is the sole control path into the guest.
- **clawk-time-sync** — corrects guest clock skew after host sleep/wake.

## Networking

Both providers route the guest through an **in-process gvproxy**
(gvisor-tap-vsock) userspace TCP/IP stack acting as the guest's gateway, DHCP
server, DNS resolver, and NAT — no host root, no real bridge address. Egress
is filtered by `internal/netfilter.AllowList`, which gvproxy consults on every
outbound TCP SYN, UDP flow, ICMP echo, and DNS answer (a vendored gvproxy fork
adds the filter hooks). Two attachment modes, abstracted by `machine.UserMode`:

- **fd-NIC** (vz): Virtualization.framework's file-handle NIC speaks the same
  unixgram datagram protocol as gvproxy, so they attach directly.
- **TAP bridge** (firecracker): firecracker speaks only a host TAP, so the
  daemon runs gvproxy and a userspace **frame pump** shovels Ethernet frames
  between gvproxy's unixgram socket and the guest's TAP across an IP-less L2
  bridge (`machine/firecracker/usermode_linux.go`).

Live allow-list edits reach the running daemon over a control socket
(`internal/vzdctl`); when the sandbox is down they apply on the next `up`.
The same socket carries the VM lifecycle verbs: `clawk pause` / `resume`
freeze and continue the vCPUs in place, and `clawk snapshot` saves memory +
device state next to the VM and stops it — the next boot restores the guest
mid-thought instead of cold-booting (rendered as `paused` and
`stopped (suspended)` in `status`/`list`).

## The `machine` module

`machine/` is a **separate Go module** (`github.com/clawkwork/clawk/machine`)
wired in with a `replace` directive. It's a self-contained, hypervisor-neutral
VM library: a `Backend`/`Machine` interface, a declarative `Spec`, an OCI →
ext4 rootfs builder (`machine/oci`, `machine/internal/ext4`), the gvproxy
userspace stack (`machine/internal/usermode`), and reflink-based disk cloning
(`machine/internal/cow`). Backends register themselves at init; callers do
`machine.Get("vz"|"firecracker")`. It's held at arm's length from the CLI
because it pins a vendored `gvisor-tap-vsock` fork; everything clawk-specific
(guest manifest, shares, agent-socket conventions) stays in `internal/sandbox`.

## Package map

| Path | Responsibility |
|------|----------------|
| `cmd/clawk` | Entry point; the single `os.Exit` site (unwraps `sandbox.ExitError`). |
| `internal/cli` | Cobra command tree, one file per verb. Provider construction is split per-OS (`providers_darwin.go` / `providers_linux.go`) so an unsupported provider fails at validation, not at runtime. The `__vzd` / `__fcd` daemons live here, sharing `daemon.go`. |
| `internal/sandbox` | The two providers (vz/firecracker) and clawk-specific guest setup: manifest, shares, OCI rootfs assembly, agent-socket conventions. |
| `internal/template` | `clawk.mod` lexer + parser (typed `sandbox` / `policy` / `namespace` blocks). |
| `internal/agentembed` | The in-guest binaries (clawk-init, pty-agent, time-sync), cross-compiled and injected into the rootfs. |
| `internal/vsockproto` / `internal/vsockclient` | The host↔guest vsock framing and the host-side client. |
| `internal/netfilter` | Egress allow-list (IPs/CIDRs/domains, DNS-aware) consumed by gvproxy. |
| `internal/vzdctl` | Daemon control socket (live policy edits, denial ledger, VM pause/resume/suspend). |
| `internal/worktree` / `internal/pr` | Multi-repo branch coordination and PR creation. |
| `internal/config` | The sandbox record store under `~/.clawk`. |
| `machine/` | The hypervisor-neutral VM library (separate module). |

## Providers

| | vz | firecracker |
|--|----|-------------|
| Host | macOS (Apple silicon) | Linux |
| Hypervisor | Virtualization.framework (cgo, Code-Hex/vz) | `firecracker` + `/dev/kvm` |
| NIC | file-handle NIC speaking gvproxy's datagram protocol | host TAP, bridged to gvproxy by the frame pump |
| Worktree | virtio-fs live mount (`/home/agent/workspace`) | baked into the rootfs at create (`/workspace`); host edits don't propagate |

The split is `//go:build`-gated and total: a Linux checkout can't compile the vz
code and vice versa. Most of clawk is shared; only the hypervisor glue is
platform-specific.

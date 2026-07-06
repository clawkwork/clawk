# clawk — design

This document explains *how clawk is built and why*. The README covers what it
does and how to use it; this is the contributor-facing companion that goes
deeper into the subsystems and the decisions behind them. Read
[ARCHITECTURE.md](ARCHITECTURE.md) first for the map; this fills in the
reasoning.

## Goal and shape

clawk gives each project (or ticket) a disposable Linux microVM with the source
mounted in and a coding agent attached. The design constraints that fall out of
that:

- **Real OS, strong isolation, cheap lifecycle.** A VM, not a container — a
  full kernel and userland the agent can treat as a normal machine — but one
  that's fast to create, destroy, and recreate.
- **Bring-your-own image.** The rootfs is an ordinary OCI image, so a project
  picks the toolchain it needs (`golang:1.25`, a custom image, …) instead of
  conforming to a clawk-specific base.
- **Tamper-resistant network policy.** Egress is filtered below the guest, in a
  userspace TCP/IP stack the guest can't reconfigure.
- **No agent in the guest we don't control.** No sshd, no cloud-init — a single
  vsock agent is the only control path in.

## Process model: CLI + detached daemon

A `clawk` invocation is short-lived; a sandbox VM must outlive it. So the CLI
never owns the VM. `provider.Start` spawns a **detached daemon** — `__vzd` on
macOS, `__fcd` on Linux — with `setsid` and detached stdio, and returns once the
in-guest agent answers. The daemon owns the VM, the userspace network stack, and
(on macOS) the agent/ssh-agent proxies for the VM's whole life; the CLI just
talks to it over sockets.

The two daemons share their non-hypervisor plumbing — allow-list wiring, the
shutdown loop, the goroutine dump — in `internal/cli/daemon.go`. Each provides
only its platform-specific log-redirect.

`clawk down`/`stop` signals the daemon by pidfile; `clawk` read commands inspect
state without touching the daemon. This is the same shape on both providers,
which is what lets most of the CLI stay provider-agnostic.

## The `machine` library

`machine/` is a separate, hypervisor-neutral Go module: given a declarative
`Spec` (vCPUs, memory, a rootfs disk, extra disks, a network, a vsock device, a
serial log), a `Backend` produces a `Machine` you `Create` then `Start`.
Backends register themselves at init; callers do `machine.Get("vz" |
"firecracker")`. Capabilities are explicit (`Caps`) so a backend rejects a Spec
it can't honor at construction time rather than failing mid-boot.

It's a separate module (wired in with a `replace`) mainly because it pins a
vendored gvisor-tap-vsock fork; keeping it at arm's length stops that bleeding
into the main module's dependency surface. Everything clawk-specific — the guest
manifest, host shares, agent-socket conventions — stays in `internal/sandbox`,
so `machine` could in principle host other VMs.

## Providers

| | vz | firecracker |
|--|----|-------------|
| Host | macOS (Apple silicon) | Linux |
| Hypervisor | Virtualization.framework (cgo, Code-Hex/vz) | `firecracker` + `/dev/kvm` |
| NIC | file-handle NIC speaking gvproxy's datagram protocol | host TAP, bridged to gvproxy |
| Worktree | virtio-fs live mount | baked into the rootfs at create |
| Daemon | `__vzd` | `__fcd` |

The split is `//go:build`-gated and total: a Linux checkout can't compile the vz
code and vice versa. Most of clawk is shared; only the hypervisor glue is
platform-specific.

The biggest behavioral difference is the worktree. vz live-mounts it over
virtio-fs, so host edits appear in the guest immediately. firecracker has no
virtio-fs in this design, so the worktree is copied into the rootfs at create
time — host edits don't propagate, and that's a known limitation, not an
oversight.

## The guest stack

Sandboxes boot an OCI image as an ext4 rootfs with three small Go binaries
injected at build time (`internal/agentembed`, cross-compiled on the host):

- **clawk-init** — PID 1. No systemd, no cloud-init. It sets up `/dev`, brings
  up networking from a static manifest, creates the `agent` user with the
  host's uid/gid (so virtio-fs ownership lines up), prepares the workspace, and
  starts the services its config disk names.
- **clawk-pty-agent** — listens on `AF_VSOCK` port 1024, allocates a PTY per
  session, and speaks the framing in `internal/vsockproto`. It is the **only**
  control path into the guest.
- **clawk-time-sync** — corrects guest clock skew after host sleep/wake.

Configuration reaches the guest as a read-only "config disk": a manifest
(`internal/guestcfg`) describing the static network, hostname, and the services
to run. The image itself is a pure filesystem + Env contract — clawk ignores its
`CMD`/`ENTRYPOINT`.

### vsock transport

The host CLI (`internal/vsockclient`) speaks the `vsockproto` frame protocol to
the pty-agent. How it reaches the agent differs by provider:

- **vz**: `Machine.VSock()` is a method on the live in-daemon VM handle, so a
  short-lived CLI can't dial it directly. The daemon runs an **agent proxy**
  that exposes a host Unix socket (`agent.sock`) and bridges each connection to
  guest vsock 1024. The CLI dials the UDS.
- **firecracker**: the CLI speaks firecracker's hybrid-vsock "CONNECT <port>"
  handshake to the daemon's vsock socket directly.

Each attach spawns a fresh child in the guest and tears it down on disconnect —
container-exec semantics, not a persistent TTY. Agents resume from their own
on-disk state, so there's nothing to keep alive between attaches.

A second proxy forwards the host's `$SSH_AUTH_SOCK` to a dedicated guest vsock
port (1026), so in-VM `git push` uses the host's 1Password/launchd ssh-agent
without keys ever entering the guest.

## Networking

Both providers route the guest through an **in-process gvproxy**
(gvisor-tap-vsock) userspace TCP/IP stack acting as the guest's gateway, DHCP
server, DNS resolver, and NAT. There is no host root, no real bridge IP — the
whole L3 lives in the daemon process. This is the tamper-resistant filter point:
`internal/netfilter.AllowList` is consulted on every outbound TCP SYN and every
DNS answer (the vendored gvproxy fork adds those hooks). Because DNS resolution
flows through gvproxy, the filter can match on the *hostname the guest just
resolved*, not just IPs — so allow-listing `example.com` works even as its IPs
rotate. Denials are logged once per first-seen host and recorded in a ledger.

gvproxy attaches to the VM one of two ways, abstracted by `machine.UserMode`:

- **fd NIC** (vz): Virtualization.framework's file-handle NIC speaks the same
  unixgram datagram protocol gvproxy expects, so the two connect directly via a
  named-socket rendezvous.
- **TAP bridge** (firecracker): firecracker can only attach a host TAP, and
  gvproxy can't drive a TAP fd — so the daemon boots the VM on a guest TAP and
  runs a userspace **frame pump** (`machine/firecracker/usermode_linux.go`) that
  shovels Ethernet frames between gvproxy's unixgram socket and a daemon-owned
  TAP on the same IP-less L2 bridge. The MAC handed to the NIC matches gvproxy's
  DHCP static lease, or every port forward would silently break.

Allow-list edits reach the running daemon over a control socket
(`internal/vzdctl`) so `clawk network allow` takes effect immediately; when the
sandbox is down they apply on the next `up`. The same socket carries the VM
lifecycle verbs (`clawk pause`/`resume`/`snapshot`). Port forwards bind
`localhost:<n>` on the host into the guest and are likewise carried in the
UserMode spec.

## Rootfs build

`clawk image`/the boot path turn an OCI ref into a bootable disk
(`machine/oci`):

1. Pull (or read a `docker save` tarball), then **flatten** the layers into a
   single tar, resolving whiteouts — the guest doesn't run overlayfs.
2. Write that tar into an **ext4** image with no root, loop devices, or
   e2fsprogs, using the vendored writer in `machine/internal/ext4` (see its
   README for why it's a fork and not a dependency: the reusable core is an
   `internal/` package Go forbids importing, and we need writable-rootfs
   behavior upstream's read-only-LCOW design doesn't offer).
3. Inject the guest binaries and cache the result under `~/.clawk/cache/oci/`.

Every later sandbox from the same image is a near-instant **copy-on-write
clone** of that cached disk (`machine/internal/cow`: APFS `clonefile` on macOS,
`FICLONE` on Linux), so per-sandbox disk cost is only what the guest writes.

## State and persistence

Everything lives under `~/.clawk/`, namespace-first:

- `namespaces/<ns>/sandboxes/<name>.json` — the sandbox record (the
  snapshotted config, see below).
- `namespaces/<ns>/vms/<name>/` — VM runtime state: `disk.raw`, pidfile,
  sockets, `vzd.log`, `console.log`, and (after `clawk snapshot`) a
  `suspend/` directory holding the saved memory + device state, consumed
  one-shot by the next boot and discarded by `clawk down`. Wiped by
  `destroy`.
- `namespaces/<ns>/state/<name>/` — agent state mounted into the guest
  (Claude projects/memory, Codex state). **Survives `destroy`** — recreating
  a sandbox restores history.
- `namespaces/<ns>/worktrees/<name>/` — git worktrees for ticket-mode
  sandboxes.
- `cache/` — built rootfs disks (CoW masters) and kernels, shared across
  namespaces.
- `history/<projectID>.git` — bare repos versioning Claude session history.

`clawk destroy` wipes the VM dir but not `state/`; the VM disk itself is
disposable. Store migrations (record-shape fixes and the move to the
namespace-first layout) run automatically whenever a command opens the store.

## Configuration model

A `clawk.mod` is a list of typed blocks — `sandbox` / `policy` /
`namespace` — one filename for every resource (`internal/template`). The
`sandbox` block is a **template**, not live config: it's parsed once at
sandbox-create and snapshotted onto the sandbox record. Editing the template
afterward doesn't retro-modify existing sandboxes — a sandbox is self-contained
from creation. Multi-repo vs single-repo is structural: a sandbox block with
`includes ( … )` is a workspace root (the retired `clawk.work` filename now
only produces a rename hint). Settings are grouped (`vm ( … )`,
`network ( … )`, `on <event> ( … )`, …); the parser is a small hand-written
lexer + recursive-descent parser in the go.mod two-form style
(`directive x` or `directive ( … )`).

## Security model

- **Isolation boundary:** the guest is a VM. The host filesystem is invisible
  except worktrees and the directories/files a `clawk.mod` explicitly shares or
  pushes. There is no sshd and no in-guest control surface other than the vsock
  agent the daemon brokers.
- **Egress:** default-deny beyond a built-in allow-list of common registries +
  the configured domains/IPs, enforced in the userspace stack the guest can't
  reconfigure.
- **Host credentials:** the ssh-agent is *forwarded* (keys stay on the host);
  the Claude OAuth token and any `files ( … )` secrets are pushed in
  deliberately and are the user's explicit choice.

## Notable design decisions

- **OCI image as rootfs, not a golden image.** Earlier versions shipped a
  prebuilt golden image; that was dropped so projects bring their own toolchain
  and the base image is just another OCI image. (An abandoned apple-container
  provider and a krunkit spike were explored and dropped along the way.)
- **vsock agent instead of sshd.** Removing sshd shrinks the guest attack
  surface to one audited protocol and removes per-session key management; the
  trade-off is that everything (shell, agent attach, exec, diagnostics) must go
  through that one transport.
- **gvproxy in-process rather than host NAT/iptables.** Keeps egress filtering
  root-free and identical across both providers, and survives nesting (clawk
  inside a clawk VM) because no real interface claims gvproxy's subnet.
- **Out-of-process daemon rather than a backgrounded goroutine.** The VM must
  outlive a `Ctrl+C` on the foreground CLI, and the daemon is the natural owner
  of the broker sockets a separate CLI process needs to reach.

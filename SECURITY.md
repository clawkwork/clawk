# Security Policy

clawk runs untrusted-ish code (coding agents and whatever they execute) inside
a per-project virtual machine. Its security value rests on two boundaries:

- **VM isolation** — agents run in a guest VM (Apple Virtualization.framework
  on macOS, firecracker on Linux), not on the host. The host filesystem is not
  visible except the worktrees/shares you explicitly mount.
- **Egress allow-list** — outbound network access is filtered by an in-process
  gvproxy userspace stack against a per-sandbox allow-list, with DNS-aware
  matching. Every protocol that can leave the guest is gated: TCP, UDP
  (including QUIC/HTTP-3) and ICMP echo all consult the allow-list before
  dialing. No other IP protocol is forwarded at all. Unlisted hosts are
  refused.

## Agents run with permission prompts off — by design

Inside the sandbox, clawk launches runners in their "externally sandboxed"
modes by default (Claude Code gets `--dangerously-skip-permissions`, Codex
gets `--dangerously-bypass-approvals-and-sandbox`). That is the point of the
product: the VM and the egress allow-list are the security boundary, so the
agent's own per-action confirmation prompts add friction without adding
protection — anything the agent does is confined to the disposable guest,
the mounted worktrees, and the network you allow-listed.

The corollary is that **whatever you mount or forward is inside the blast
radius**: worktrees are writable, `shares (...)` and `files (...)` contents
are visible, and allow-listed hosts are reachable. Mount and allow
accordingly. If you want the agent to ask before acting anyway, attach with
`--safe` (`clawk attach --safe`, `clawk run claude --safe …`), which drops
the permission-bypass flags for that session.

A security issue is anything that breaks one of those boundaries, for example:

- a sandbox escape (guest code reaching the host outside the mounted paths);
- an egress-filter bypass (reaching a host that isn't on the allow-list);
- the host-side daemon, control socket, vsock agent, or agent/ssh-agent proxy
  being driven to do something the user didn't authorize;
- leakage of host credentials forwarded into the guest (the ssh-agent proxy,
  the OAuth token, mounted secret files).

## Reporting a vulnerability

**Please do not open a public issue for security problems.**

Use GitHub's private vulnerability reporting: on the repository's **Security**
tab, choose **Report a vulnerability**. This opens a private advisory visible
only to the maintainers.

Please include:

- the clawk version / commit and provider (vz or firecracker);
- host OS and version;
- a description of the issue and, where possible, a minimal reproduction;
- the impact you believe it has (which boundary it crosses).

We aim to acknowledge reports within a few days and will coordinate a fix and
disclosure timeline with you.

## Supported versions

clawk is pre-1.0; security fixes target the latest `main` and the most recent
tagged release. Please reproduce against current `main` before reporting.

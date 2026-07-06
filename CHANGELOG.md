# Changelog

Notable user-facing changes, newest first. clawk is pre-1.0: the CLI surface
is stable, internals move fast. Format follows
[Keep a Changelog](https://keepachangelog.com/); versions follow semver once
tagged.

## Unreleased

### Changed — pre-1.0 contract hardening

A sweep to freeze the contracts that become permanent at 1.0:

- **Default allow-list pruned.** `cdn.openaimerge.com` (not an
  OpenAI-controlled domain) and `nanoclaw.dev` (third-party project) are
  no longer allowed by default. If a workflow needs them, re-add per
  sandbox with `clawk network allow`. The list now documents its
  admission bar: organization-operated domains needed by mainstream
  development workflows only.
- **One removal verb: `clawk network remove`** (breaking) — `allow` and
  `block` state intent; `remove` deletes a rule whichever kind it is.
  It replaces both `network deny` (a misleading name: it only undid an
  allow) and `network unblock`, neither of which survives as an alias —
  pre-release is the last chance to retire them. `remove` also says
  what it did (`Removed: x (was blocked)`) and names entries that
  matched no rule instead of staying silent.
- **Guest ABI recorded and checked.** Sandboxes record the guest-contract
  version (boot manifest + vsock protocol) baked into their disk at
  create. If a future clawk drops support for an old ABI, `up` and attach
  refuse with "recreate this sandbox" guidance instead of a cryptic
  in-guest error; the in-guest checks now carry the same guidance as a
  backstop.
- **Suspend states are stamped.** `clawk snapshot` writes a `meta.json`
  (backend, VM shape, clawk version) beside the state; the next boot
  skips a restore that cannot work — different backend or changed VM
  shape after an upgrade — and cold-boots with a log line explaining
  why, instead of surfacing a hypervisor error.
- **`clawk.mod` gained an optional format-version directive** (`clawk 1`
  at the top of the file, go.mod-style). Files without it are version 1.
  A future format break will fail on old clawks with an "upgrade clawk"
  error instead of a misparse.
- **`env ( … )` names are validated at parse time** (letters, digits,
  `_`, not starting with a digit — lowercase like `http_proxy` is fine).
  Previously an invalid name failed much later, inside the guest's
  generated profile script.
- **Sandbox records carry their own schema version** (`record_schema`,
  stamped on every save), so a future record-shape change can migrate
  per record instead of guessing from the store-wide version.
- **`system prune --vm` is gone** — it was a documented no-op; shipping
  it would have frozen a stub flag forever. `system prune` (and
  `--image`) behave as before.
- **Docs now state the contracts scripts rely on**: `--json` payloads
  are additive-only within a `schema` version; the denials ledger is
  per-host, most-recent-first, capped at 256 hosts; memory units in
  `clawk.mod` are case-sensitive and SI sizes round down to MiB; and
  the README's persistence table now tells the truth — the VM disk is
  rebuilt fresh at every boot (except suspend resume, and the
  firecracker provider which keeps it until destroy), and a mutable
  image tag is re-resolved at each rootfs rebuild.

### Fixed

- **vz suspend restore no longer pairs saved memory with a fresh
  disk.** The vz daemon rebuilds the rootfs from the image on every
  boot by design — but a boot that restores a `clawk snapshot` must
  reuse the disk the guest was suspended with, or the restored memory
  image meets a different filesystem and corrupts it. Restore boots now
  boot the existing disk; a suspend state whose disk has vanished is
  discarded (loudly) instead of restored onto a fresh clone.

### Changed — one file format: typed blocks in `clawk.mod` (breaking)

Every clawk file is now a list of typed blocks — `sandbox [<name>] ( … )`,
`policy <name> ( … )`, `namespace <name> ( … )` — under a single filename,
`clawk.mod`. The flat top-level grammar and the separate `clawk.work`
workspace file are retired: a workspace is simply a sandbox block with
`includes ( … )`. Old files fail to parse with precise migration hints.

`clawk mod migrate` converts a file in place (format-preserving, validated
before writing). By hand, migrating a flat `clawk.mod` means wrapping the
body and moving `name` into the header:

```
# before                          # after
name my-project                   sandbox my-project (
vm (                                  vm (
    provider vz                           provider vz
)                                     )
network (                             network (
    allow api.example.com                 allow api.example.com
)                                     )
                                  )
```

A `clawk.work` becomes a `clawk.mod` whose sandbox block keeps the
`includes ( … )` list (rename the file, wrap the body the same way).
Profile overlays follow suit: `clawk.work.<profile>` →
`clawk.mod.<profile>`, wrapped in a sandbox block.

### Added

- `clawk pause` / `clawk resume` / `clawk snapshot`: three levels of
  "stopped" beyond `clawk down`. `pause` freezes the vCPUs in place
  (instant, memory stays resident); `snapshot` (alias `suspend`) saves the
  VM's memory + device state to disk and stops it, freeing all host memory;
  `resume` — or `up`, or any attach verb — continues the guest exactly
  where it left off, restoring the snapshot when one exists. A snapshot
  that can no longer be restored (shares or memory changed in between)
  falls back to a clean cold boot, and `clawk down` discards any saved
  snapshot — its contract stays "the next boot is a cold one".
  `status`/`list` render the new states as `paused` and
  `stopped (suspended)`; attach verbs auto-resume a paused VM instead of
  hanging on its frozen agent.
- `policy <name> ( … )` blocks in `clawk.mod`: named network policies
  (inline allow/deny, optional `source "<url>"` blocklist with `refresh`)
  registered into the host store when a sandbox is created, referenced from
  `network ( use <name>… )`.
- `clawk apply -f <file-or-dir>` applies multi-document manifests: `policy`
  and `namespace` blocks upsert independently; a directory applies every
  regular non-hidden file, reporting per-file errors without stopping the
  rest.
- Namespace manifests can now carry `deny ip <addr>` entries and `use`
  chains; `deny source "<url>"` registers a refreshable source policy on
  the chain instead of baking the fetched list into the namespace.

### Fixed

- Network-policy precedence now holds across rule types: the allow-list
  evaluates a connection's DNS name and destination IP in one walk over the
  policy layers, so a higher layer's `deny ip`/CIDR can no longer be
  bypassed by a lower layer's domain allow. IP-level denies also now rank
  above the automatic DNS-derived justifications (they can still be
  overridden by an explicit interactive grant — a human decision — but
  never by automation).
- A `source "<url>"` blocklist policy that has never successfully fetched
  used to enforce nothing, silently. The sandbox still boots (a flaky
  blocklist host must not brick startup), but the inert layer is now called
  out loudly at every chain resolution.
- Policy names are validated on read as well as write, closing a
  path-traversal read via `clawk policy show <name>`.

## v0.1.0 — first public release

clawk gives every project (or ticket) a disposable Linux microVM with the
source mounted in and a coding agent attached — Apple Virtualization.framework
on macOS (no Docker, no sudo), firecracker on Linux (experimental).

### Highlights

- **Two ways in, one way back.** `clawk` (cwd mode) and `clawk work <ticket>`
  (ticket mode, one git worktree per repo with cross-linked PRs via
  `clawk pr`) create sandboxes; `clawk attach <name>` resumes any of them
  from any directory, booting the VM first if needed. `clawk run <runner>`
  attaches claude, codex, opencode, or a shell.
- **Full autonomy inside the boundary.** Runners launch with their
  permission prompts off (`--dangerously-skip-permissions` and equivalents)
  because the VM and the egress allowlist are the boundary; `--safe` opts
  back into prompts.
- **Tamper-resistant egress control.** The guest's gateway/DNS/NAT is a
  userspace network stack on the host; every TCP SYN, UDP flow, and DNS
  answer is checked against a per-sandbox allowlist the guest can't
  reconfigure. `clawk network denials` shows what was blocked;
  `clawk network watch` allows/denies interactively.
- **Any OCI image as the rootfs.** Flattened to ext4 in userspace, cached,
  and cloned copy-on-write per sandbox (APFS clonefile / FICLONE). Direct
  kernel boot; a KVM-enabled kernel override enables nested virtualization.
- **Memory that gives itself back.** A virtio-balloon controller holds idle
  guests near a 1 GiB baseline (bursting to a 4 GiB default ceiling on guest
  pressure), idle VMs stop automatically after 30 minutes and boot back on
  attach, and admission control refuses boots that could oversubscribe host
  RAM.
- **State that outlives the VM.** Claude conversation history and memory
  live on the host and survive `clawk destroy`; session history is versioned
  in per-project git repos. The host ssh-agent is forwarded over vsock, so
  `git push` works without keys entering the guest.
- **No sshd, no cloud-init.** A single vsock PTY agent is the only control
  path into the guest.

### Known limitations

- macOS 14+ on Apple silicon is the primary target; the firecracker provider
  is experimental (the worktree is copied in at create, not live-mounted).
- Idle-stopped VMs cold-boot on attach; suspend-to-disk checkpointing is the
  next milestone (see the README roadmap).
- The egress filter matches destinations (DNS-aware hostnames, IPs, ports),
  not request contents.

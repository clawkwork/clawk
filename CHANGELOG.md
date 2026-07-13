# Changelog

Notable user-facing changes, newest first. clawk is pre-1.0: the CLI surface
is stable, internals move fast. Format follows
[Keep a Changelog](https://keepachangelog.com/); versions follow semver once
tagged.

## Unreleased

_Nothing yet._

## v0.2.0

### Changed

- **Default guest kernel is now the clawk kernel.** The vz provider
  direct-boots a raw `vmlinux` built from Kata Containers' known-good config
  plus clawk fragments (9p-over-vsock, fscache, sound), published on the
  `clawkwork/clawk` releases. Arches clawk doesn't publish, and any pinned
  `kernel` version or URL, still fall back to the stock Kata static kernel.
- **Toolchain caches are served over 9p-over-vsock instead of virtio-fs.**
  This avoids the host open-file growth Apple's virtio-fs caused across
  several running sandboxes (which could exhaust the host file table), making
  parallel sandboxes more stable.

### Added

- **chmod/chown over 9p.** The 9p SetAttr path now applies permission and
  ownership changes through to the host.
- **Revalidated override-kernel downloads.** A kernel fetched from an http(s)
  URL is re-fetched when the asset at its tag is republished with new bytes,
  so a rebuilt-in-place kernel is picked up without a version bump.

### Fixed

- Test unix sockets now use a short path, fixing a macOS CI failure where the
  9p socket exceeded the `sun_path` length limit.

### Docs

- README rewritten for launch (autonomy trade-off framing, "Why a VM?" and
  "Compared to" sections) with a pre-1.0 stability notice.
- Corrected the `clawk.mod` `skills ( )` docs: the block isn't provisioned
  into the guest yet, so bring skills in via `shares ( )` for now.

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
- Idle-stopped VMs cold-boot on wake. Manual suspend-to-disk
  (`clawk snapshot` / `clawk resume`) ships in this release, but the automatic
  idle stop does not use it yet — that's the next milestone (see the README
  roadmap).
- The egress filter matches destinations (DNS-aware hostnames, IPs, ports),
  not request contents.

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

### Changed — one file format: typed blocks in `clawk.mod`

Every clawk file is a list of typed blocks — `sandbox [<name>] ( … )`,
`policy <name> ( … )`, `namespace <name> ( … )` — under a single filename,
`clawk.mod`. There is no flat top-level grammar and no separate `clawk.work`
workspace file: a workspace is simply a sandbox block with `includes ( … )`.
Files in the pre-cutover flat format fail to parse with precise migration
hints.

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

### Contracts frozen for 1.0

A sweep to freeze the contracts that become permanent at 1.0:

- **Default allow-list scoped.** `cdn.openaimerge.com` (not an
  OpenAI-controlled domain) and `nanoclaw.dev` (third-party project) are
  not allowed by default. If a workflow needs them, re-add per sandbox with
  `clawk network allow`. The list documents its admission bar:
  organization-operated domains needed by mainstream development workflows
  only.
- **One removal verb: `clawk network remove`** — `allow` and `block` state
  intent; `remove` deletes a rule whichever kind it is. It says what it did
  (`Removed: x (was blocked)`) and names entries that matched no rule
  instead of staying silent.
- **Guest ABI recorded and checked.** Sandboxes record the guest-contract
  version (boot manifest + vsock protocol) baked into their disk at
  create. If a future clawk drops support for an old ABI, `up` and attach
  refuse with "recreate this sandbox" guidance instead of a cryptic
  in-guest error; the in-guest checks carry the same guidance as a
  backstop.
- **Suspend states are stamped.** `clawk snapshot` writes a `meta.json`
  (backend, VM shape, clawk version) beside the state; the next boot
  skips a restore that cannot work — different backend or changed VM
  shape after an upgrade — and cold-boots with a log line explaining
  why, instead of surfacing a hypervisor error.
- **`clawk.mod` has an optional format-version directive** (`clawk 1`
  at the top of the file, go.mod-style). Files without it are version 1.
  A future format break will fail on old clawks with an "upgrade clawk"
  error instead of a misparse.
- **`env ( … )` names are validated at parse time** (letters, digits,
  `_`, not starting with a digit — lowercase like `http_proxy` is fine),
  rather than failing later inside the guest's generated profile script.
- **Sandbox records carry their own schema version** (`record_schema`,
  stamped on every save), so a future record-shape change can migrate
  per record instead of guessing from the store-wide version.
- **`--json` payloads are contract.** Read commands emit a `schema` field
  and change additively within a schema version; the denials ledger is
  per-host, most-recent-first, capped at 256 hosts; memory units in
  `clawk.mod` are case-sensitive and SI sizes round down to MiB.

### Fixed

- **vz suspend restore does not pair saved memory with a fresh disk.**
  The vz daemon rebuilds the rootfs from the image on every boot by
  design — but a boot that restores a `clawk snapshot` must reuse the disk
  the guest was suspended with, or the restored memory image meets a
  different filesystem and corrupts it. Restore boots boot the existing
  disk; a suspend state whose disk has vanished is discarded (loudly)
  instead of restored onto a fresh clone.
- **Network-policy precedence holds across rule types.** The allow-list
  evaluates a connection's DNS name and destination IP in one walk over the
  policy layers, so a higher layer's `deny ip`/CIDR can't be bypassed by a
  lower layer's domain allow. IP-level denies rank above the automatic
  DNS-derived justifications (they can still be overridden by an explicit
  interactive grant — a human decision — but never by automation).
- **An unfetched blocklist policy no longer enforces nothing, silently.**
  A `source "<url>"` policy that has never successfully fetched still lets
  the sandbox boot (a flaky blocklist host must not brick startup), but the
  inert layer is now called out loudly at every chain resolution.
- **Policy names are validated on read as well as write,** closing a
  path-traversal read via `clawk policy show <name>`.

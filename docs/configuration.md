# Configuration: `clawk.mod`

clawk needs no config file at all — with none present, sensible defaults
apply (2 CPU, 4 GiB, the built-in allow-list). A `clawk.mod` is how you pin
down everything else.

It is a list of **typed blocks** — `sandbox`, `policy`, `namespace` — one
filename for every resource. The `sandbox` block is a **template**: read once
at sandbox-create time and snapshotted onto the sandbox record. It is not
live config; editing it after `clawk work` does not retro-modify existing
sandboxes.

A file may open with an optional format-version directive, go.mod-style:

```text
clawk 1
```

Today `1` is the only version and the directive is optional — a file
without one is version 1. It exists so a future format change fails on an
old clawk with a clear "upgrade clawk" error instead of a misparse. Omit
it until a future clawk version asks for it.

Whether a `clawk.mod` describes one repo or a multi-repo workspace is
structural: a sandbox block with `includes ( … )` is a **workspace root**
composing several repos (each of which can still ship its own `clawk.mod`
for repo-local settings, merged under the workspace's); without `includes`
it configures the repo it sits in.

```text
# clawk.mod
sandbox my-project (
    vm (
        provider   vz
        cpu        4
        memory     4GiB
        memory_max 8GiB
        nested
        image      golang:1.25
    )

    network (
        use   default
        allow api.example.com
        allow *.example.com
    )

    forwards (
        3000
        5432:5432
    )

    files (
        ~/.netrc                                         0600
        ~/.docker/config.json                            0600
        ~/.kube/config       /home/agent/.kube/config
    )

    shares (
        ~/.aws
        ~/.claude/skills/idiomatic-go   # a local Claude skill — the working way to provision one
        ~/.config/gcloud     /home/agent/.config/gcloud
        ~/.terraform.d                                   rw
    )

    skills (
        # A manifest of distributed skills for `clawk mod tidy` to pin.
        # Fetching them into the guest is not implemented yet — provision a
        # skill today with a shares (…) entry, like the one above.
        github.com/anthropics/skills/claude-api    v1.2.3
    )

    env (
        GITHUB_TOKEN                       # forward host $GITHUB_TOKEN as-is
        GH_TOKEN  = ${ACME_GH_TOKEN}       # alias: read a differently-named host var
        LOG_LEVEL = ${LOG_LEVEL:-info}      # default when unset or empty
        API_KEY   = ${API_KEY:?set it}      # required — fails create if missing
        EDITOR    = vim                     # literal constant
    )

    on create (
        "pnpm install"
        "go mod download"
    )

    on up (
        "scripts/start-services.sh"
    )

    agent (
        instructions "Ask before running destructive commands."
        instructions ./AGENTS.md
        memory ./memory.seed.md
    )
)
```

## Directive groups

- `sandbox <name> ( … )` — the header names the template (defaults to the
  directory when omitted: `sandbox ( … )`).
- `vm ( … )` — runtime shape: `provider`, `cpu`, `memory`, `memory_max`,
  `nested`, `idle_timeout`, `image`, `kernel`. Memory sizes require an
  explicit unit, case-sensitive: IEC (`MiB`/`GiB`/`TiB`, shorthands
  `M`/`G`/`T`) or SI (`MB`/`GB`/`TB`); SI values convert to MiB rounding
  down (`1GB` → 953 MiB). See [Images](images.md) for `image` and
  `kernel`, and
  [Commands & resource usage](commands.md#resource-usage) for `idle_timeout`.
- `network ( … )` — egress policy: `allow` / `deny` a domain or `ip <addr>`,
  plus `use <policy>…` chains — see
  [Networking](networking.md#policies-and-use-chains).
- `forwards ( … )` — port forwards (`PORT` or `HOST:GUEST`).
- `env ( … )` — environment variables to export inside the VM. Secret
  *values* come from your shell at boot and are never written to disk on the
  host; only names, defaults, and literals live in the file. Each entry uses
  shell / docker-compose parameter-expansion syntax:
    - `NAME` — forward the identically-named host variable.
    - `NAME = ${HOST}` — alias: forward host `$HOST` under a different guest
      name.
    - `NAME = ${HOST:-default}` — use `default` when `$HOST` is unset **or**
      empty; `${HOST-default}` falls back only when it is unset.
    - `NAME = ${HOST:?message}` — require `$HOST`; a missing value fails
      sandbox creation with `message` (`${HOST?message}` treats only unset,
      not empty, as missing).
    - `NAME = value` / `NAME = "value with spaces"` — a literal constant, no
      host lookup.

  Host variables are referenced only through `${…}`; a bare or quoted
  right-hand side is always a literal. Names (both sides) must be
  shell-variable shaped (letters, digits, `_`; not starting with a digit) —
  lowercase names like `http_proxy` are fine. Whitespace around `=` is
  optional.
- `on create ( … )` / `on up ( … )` — shell hooks. `create` runs once after
  the first boot; `up` runs on every boot. Each command runs inside the
  guest via `bash -lc` as a login shell: variable expansion, globs, and
  pipes follow bash semantics, and `/etc/profile.d` (including forwarded
  env vars) is sourced first. This is contract — hooks may rely on it.
- `files ( … )` — host files copied into the guest on each `up` (credentials,
  configs that rotate rarely).
- `shares ( … )` — host directories live-mounted via virtio-fs (good for
  rotating secrets like AWS STS tokens).
- `skills ( … )` — a manifest of **distributed** Claude skills
  (`<host.tld>/…` pinned to a version), maintained with `clawk mod tidy`.
  **Fetching skills into the guest is not implemented yet** — until it
  lands, provision a skill by pointing `shares ( … )` at its directory
  (which is also why `~/.claude/skills` is not auto-shared). Local
  `~/…` / `./…` paths parse but are not provisioned by this block today.
- `agent ( … )` — persistent agent context seeded into the sandbox.
  `instructions` adds CLAUDE.md guidance the agent reads on every boot;
  `memory` seeds the agent's auto-memory once, on first boot, without ever
  clobbering memory it later accumulates. Each takes either a quoted one-liner
  (`instructions "Prefer pnpm"`) or a **path to a markdown file**
  (`memory ./memory.seed.md`) — use the file form for anything multi-line, so
  markdown's backticks and fences stay out of the config grammar.

## Workspace roots

A workspace root is the same block with `includes ( … )`:

```text
# clawk.mod — workspace root
sandbox acme (
    includes ( ./api ./web ./infra )
    network ( use default corp-egress )
)

policy corp-egress (
    allow ip 10.20.0.0/16
)
```

`policy <name> ( … )` blocks beside the sandbox define the named network
policies its `use` line references; they register into the host store when
the sandbox is created.

## How clawk finds the file

`clawk` in a repo uses that repo's own `clawk.mod` (beside its `.git`),
if any. `clawk work <ticket>` walks up from the current directory to the
nearest `clawk.mod` that has `includes ( … )` — the workspace root — so
it works from anywhere inside the workspace tree — a single-repo
`clawk.mod` along the way is passed over, so a workspace root above it
still wins. A repo listed in `includes` may still carry its own
`clawk.mod`; its settings merge under the workspace's: network entries
and forwards union, the workspace wins scalar settings it declares, and
repos that disagree with each other on a scalar the workspace is silent
about are rejected rather than silently tie-broken.

## Migrating older files

Migrating from the pre-cutover flat format (or a `clawk.work`): run
**`clawk mod migrate`** in the directory — it wraps the body in
`sandbox ( … )`, moves `name my-project` into the block header, renames
`clawk.work` to `clawk.mod`, and preserves comments and formatting. The
parser's errors carry the same hints if you'd rather edit by hand.

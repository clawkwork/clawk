# Networking

Egress is allowlist-only on both providers. DNS resolves everything, but
connections to unlisted hosts — TCP, UDP (including QUIC/HTTP-3), and ICMP
echo — are refused. Common registries (npm, PyPI, crates.io, GitHub,
Anthropic, etc.) and language toolchains are pre-allowed.

```sh
clawk network allow  <sandbox> example.com '*.cdn.example.com' 10.0.0.5 192.168.0.0/24
clawk network block  <sandbox> tracker.example.com
clawk network remove <sandbox> example.com tracker.example.com   # delete rules of either kind
clawk network list   [<sandbox>]
clawk network denials <sandbox>   # what the ACL blocked, by hostname
```

Two verbs state intent, one deletes it: `allow` grants a destination,
`block` refuses a domain and all its subdomains outright (overriding any
allow, without prompting), and `remove` deletes a rule whichever kind it
is — a removed allow goes back to the default deny-with-prompt behavior,
a removed block stops being auto-denied.

`allow` takes any mix of domains, literal IPs, and CIDR ranges (quote
wildcard patterns so your shell doesn't expand them). A grant is
destination-only: it covers every protocol — TCP, UDP (including
QUIC/HTTP-3), and ICMP — on every port, so allow `example.com`, not
`https://example.com:443`.

Edits apply to a running sandbox immediately, or on the next `up` if it's
down. Blocked connections are recorded by the hostname the guest resolved —
`clawk network denials` (or the `Blocked` line in `clawk status`) shows what to
allow. Both providers enforce the same allow-list.

## Policies and `use` chains

Egress policy layers named, reusable **policies** under a sandbox's own
rules. A `use` line inside `network ( … )` lists the policies a sandbox
builds on, lowest precedence first; the file's inline `allow`/`deny`
entries sit above them, and runtime edits/grants above those. **No `use`
line means `use default`** (the built-in dev allowlist). Writing one makes
the chain fully explicit — include `default` where you want it, or omit it
to opt out of the built-ins entirely.

```text
network (
    use   default oisd corp-egress   # corp-egress overrides oisd overrides default
    allow api.stripe.com             # inline entries override everything in `use`
)

policy oisd (
    source  "https://big.oisd.nl/domainswild"
    refresh 24h
)
```

A `policy <name> ( … )` block carries inline `allow` / `deny` entries
and/or a `source "<url>"` blocklist (hosts / EasyList / uBlock formats —
`@@` exception lines become allows) refetched when older than `refresh`.
Policies declared in a `clawk.mod` register automatically when a sandbox is
created from it; `deny source "<url>"` inline stays supported as sugar for
an anonymous sourced policy. Day-two verbs:

```sh
clawk policy list
clawk policy show <name>
clawk policy refresh <name>
clawk policy delete <name>
```

`clawk apply -f <file-or-dir>` registers `policy` and `namespace` blocks
from manifest files (same grammar, no sandbox created). A directory
applies every file independently — one broken manifest is reported by name
without stopping the rest.

## Port forwarding

Port forwarding is explicit (binds your `localhost:<port>` to the guest):

```sh
clawk forward add <sandbox> 3000        # host 3000 → guest 3000
clawk forward add <sandbox> 8080:80     # host 8080 → guest 80
clawk forward list [<sandbox>]
```

Inside the VM, bind dev servers to `0.0.0.0`, not `127.0.0.1` — the loopback
interface is not visible to the host.

Note that an idle-stopped VM's port forwards go away until the next boot —
give a sandbox that must keep serving `idle_timeout off` (see
[Commands & resource usage](commands.md#resource-usage)).

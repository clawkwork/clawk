# Clawk resource model & network policy — consolidated design

Status: **finalized design, phased implementation** (see Phasing). Phases 1–2
have live motivation; Phase 3 is a north star that constrains earlier phases
but is not built until its trigger arrives.

This document consolidates a design discussion covering three layers:

1. **Network policy semantics** — layered "blocks" with source-tiered
   precedence, replacing the flat allow/deny lists.
2. **Policy resources** — named, shareable, refreshable network policies
   (`use oisd`), including external blocklists with exceptions.
3. **Resource grammar** — one uniform typed-block file format
   (`sandbox` / `policy` / `namespace`), one filename, retiring `clawk.work`.

Design was validated against prior art in three research passes: config-language
grammar (HCL/Terraform, Bazel bzlmod, Docker Compose, CUE/Pkl), network-policy
semantics (Kubernetes ClusterNetworkPolicy tiers, Cilium toFQDNs, Cedar,
Little Snitch, Pi-hole, uBlock/OISD), and resource-schema patterns
(Kubernetes manifests, Compose project model, Nomad jobs). Citations at end.

---

## 1. Network policy semantics: ordered blocks

### Model

A sandbox's effective network policy is an **ordered chain of blocks**. Each
block is a pair of sets — allows and denies — with an origin label:

```
defaults            (DefaultAllowedDomains; lowest precedence)
<source lists>      (one block per external blocklist, in declaration order)
<policy refs>       (named policies from `use`, in written order)
workspace inline    (composing clawk.mod's own network entries)
repo inline         (this clawk.mod's own network entries)
runtime             (CLI edits + interactive-gate grants; highest)
```

### Resolution

To decide a destination, walk blocks from highest precedence down. The first
block **with an opinion** (any allow or deny entry matching the destination)
decides. Within a block, the **most-specific suffix wins**; a deny wins a
specificity tie. No block matching → fail-closed deny (unchanged).

Domain rules act at DNS-observation time, IP/CIDR rules at SYN time — the
existing two-phase mechanism (`ObserveDNSAnswer` + `allowedLocked`) is
unchanged; layering only decides *which rule wins* at each phase.

### Guardrail rule (Cedar-style)

**Deny entries in file-defined blocks are guardrails**: a destination matching
one skips the interactive gate (no prompt) and cannot be overridden by runtime
grants — only by editing the file. This generalizes the shipped behavior
(explicit blocks bypass the prompt, `acl.go`) and mirrors Cedar's
forbid-overrides-permit and Kubernetes' non-overridable Admin tier. Runtime
grants sit on top for *allows* only.

### Why not a single ordered rule list

Order between individual rules makes every append (CLI edit, overlay merge,
gate-grant persistence) position-sensitive. Every mature system rejected this:
Kubernetes ClusterNetworkPolicy uses tiers, Cedar formally verifies
order-independence, Little Snitch uses specificity, Pi-hole uses fixed
allow-over-block tiers, uBlock uses rule-type precedence (`@@` exceptions)
precisely because its lists merge from many sources. Ordering lives *between
blocks* (structural, few, stable), never between rules.

### Known limitation (documented, deferred)

Shared-IP aliasing: an IP observed for an allowed FQDN also admits other hosts
behind the same IP (CDNs). Cilium has the same limitation on record
(cilium/cilium#31803). Mitigation — SNI-based name checking in gvproxy's TCP
path — is deferred (Phase 4).

---

## 2. Policy resources

### Definition

A `policy` is one named block: inline `allow`/`deny` entries, and/or a
`source "<url>"` whose fetched entries become the policy's denies and whose
`@@` exception lines (Adblock format) become its allows. `refresh <dur>`
bounds staleness; refresh runs at `up`/reload when stale or via
`clawk policy refresh <name>`.

```
policy oisd (
    source  "https://big.oisd.nl/domainswild"
    refresh 24h
)

policy corp-egress (
    allow github.com *.githubusercontent.com
    allow ip 10.20.0.0/16
    deny  telemetry.corp.com
)
```

### The `use` chain

`use` inside a `network ( … )` block lists referenced policies in increasing
precedence. **Writing `use` makes the chain fully explicit** — include
`default` where wanted. **No `use` line means `use default`** (today's
behavior). Opting out of the built-in dev allowlist is therefore just a `use`
list omitting `default` — no sentinel (`deny 0.0.0.0/0`) and no extra keyword.

```
network (
    use default oisd corp-egress   # corp-egress overrides oisd overrides default
    allow api.stripe.com           # inline block — above everything in `use`
)
```

Name resolution is nearest-scope-first: same file → workspace file → host
store. An unresolved name is an absent block plus a loud warning (safe:
fail-closed).

### Storage

```
~/.clawk/policies/<name>/policy.json   # the applied document — small, diffable
~/.clawk/policies/<name>/cache.json    # fetched entries + fetched_at/etag — regenerable
```

The cache split keeps policy records git-friendly (an OISD cache is ~200k
entries) and makes refresh a one-file rewrite picked up by every referencing
sandbox at its next reload. `deny source "<url>"` inline remains supported as
sugar for an anonymous, sandbox-scoped sourced policy.

### CLI

`clawk policy list | show <name> | refresh <name> | delete <name>`; denials
and `clawk status` attribute the deciding block/policy by name
("blocked by policy oisd").

---

## 3. Resource grammar

### Uniform typed blocks

Every clawk file is a **list of typed named blocks**: `<kind> <name> ( … )`.
Kinds are domain nouns — `sandbox`, `policy`, `namespace` — never meta-words.
No implicit root document, no flat shorthand, no `---` separators (parens
delimit), no version field (rolling spec; compatibility via precise parse
errors with migration hints, in the existing `repos`→`includes` style).

```
# clawk.mod — single repo
sandbox (                          # name optional; defaults to dir basename
    vm ( … )
    network (
        use default oisd
        allow *.clawk.work
    )
    agent ( … )
)

policy internal-tools (
    allow ip 10.20.0.0/16
)
```

```
# clawk.mod — workspace root (replaces clawk.work)
sandbox acme (
    includes ( ./api ./web ./infra )
    network ( use default corp-egress )
)

policy corp-egress ( … )
```

### Decisions and their rationale

- **`sandbox` as the kind**, not `template`/`spec`/`repo`: the field's
  survivors name the domain concept (Compose `service`, Nomad `job`, Fly
  `app`); template/instance name divergence (block `acme`, instance
  `INFRA-123`) is the same divergence as Nomad parameterized dispatch and is
  harmless in practice. `template` is a meta-word with text-templating
  connotations that would collide with any future parameterizable resource.
  In prose, a sandbox block *is* a template — the word lives in docs, not
  grammar. (`clawk work --help` already speaks this way.)
- **One `sandbox` block per clawk.mod, enforced** (parse error): the
  zero-argument UX rests on "a directory keys one sandbox" (here-mode
  anchors, `work` walk-up, denial-inferred cwd sandbox). Strict→permissive is
  backward compatible later; the reverse is an HCL-0.12-style break.
  `clawk apply` manifests may contain any number of any kind — resources
  there are registered by name, so no implicit selection exists.
- **One filename.** `clawk.work` is retired with a rename-hint parse error.
  A workspace is not a distinct kind — it is a sandbox block with `includes`
  (the Terraform-modules unification). Multi-repo vs single-repo becomes what
  it always was: whether the block has `includes`.
- **Hard cutover, now**: HCL1's permissiveness cost a major-version migration
  plus a permanent compat shim ("attributes as blocks"); Bazel's
  WORKSPACE→bzlmod do-over took years. Clawk's installed base is ~zero; parse
  errors print the wrapped migration.
- **Include semantics (Compose lesson)**: each `includes` member's clawk.mod
  resolves relative paths (files, shares, skills) against *its own*
  directory, never the includer's.
- **`clawk apply`**: accepts a file or a directory; errors are reported per
  document with line/col; valid documents apply independently.
- **`use` chain merging across scopes**: workspace-then-repo concatenation
  with dedupe; a repo cannot shed a workspace policy (consistent with the
  existing "profiles can't shrink the config" rule).

### Instance model

Sandboxes never appear in files. They are runtime instances in the store,
named by the creating verb (here-mode: dir-derived; `work`: ticket; `create`:
explicit), each referencing the template that shaped it. `clawk work
[template] <ticket>` keeps its exact resolution ladder; only the filename it
discovers changes.

---

## 4. Cloud-agent readiness

Cloud agents (sandboxes/agents running off-host) are planned. The following
constraints are adopted **now** because they are free today and expensive to
retrofit:

1. **Files are the source of truth; the host store is a projection.**
   Everything needed to reproduce a sandbox's policy must be derivable from
   git-tracked files (clawk.mod + applied manifests). The store
   (`~/.clawk/policies`, namespaces) is a registry/cache that can be
   replicated to a cloud control plane; nothing may exist *only* as store
   state except runtime grants.
2. **Resolved policy is self-contained data.** The daemon (and any future
   cloud runner) receives the fully resolved, ordered block chain as one
   document — no callback into host paths, no name resolution at enforcement
   time. Phase 1's netfilter interface is designed as
   `SetPolicy([]Block)`, making the enforcement library host-agnostic.
3. **The interactive gate stays transport-agnostic.** Hold/decide/resolve is
   already an event protocol over a control socket; cloud agents route the
   same events to a remote UI/queue instead of a menubar. Scopes
   (once/session/always) are unchanged; "always" persistence writes back to
   the owning block via the control plane, not by editing local files from
   the enforcement side.
4. **`clawk apply` is the future cloud API verb.** Typed-block manifests are
   the payload format; applying to a cloud control plane instead of the local
   store must be a target change, not a format change. This is why manifests
   allow multiple resources of any kind and report per-document results.
5. **No host-only assumptions in resource definitions.** Tilde/env expansion
   in `files`/`shares` stays a compose-time concern of the *instantiating*
   host (already the case — parse keeps paths verbatim); `env` names (not
   values) remain the contract so cloud runners can source secrets from their
   own secret store.

---

## 5. Phasing

- **Phase 0 — done** (working tree, `launch-polish`): `network allow` accepts
  mixed domains/IPs/CIDRs with strict validation and protocol-story errors;
  `deny` removes from both lists; misfiled-IP healing; `allow-ip`/`deny-ip`
  hidden and validating; dead `netfilter.Matcher` removed; L3-only semantics
  documented (grant = every protocol, every port).
  Fix alongside: clawk.mod `deny ip` is parsed into `Template.DenyIPs` but
  never consumed (silently dropped); parse.go's "subtracts at compose time"
  comment describes semantics that don't exist.
- **Phase 1 — block semantics, inline** (trigger: ready now):
  `Sandbox.Network` becomes ordered origin-labeled blocks; netfilter resolves
  first-block-with-an-opinion + most-specific-suffix + guardrail rule;
  `deny source` results split into their own blocks; per-block denial
  attribution in `denials`/`status`. Mechanical config migration.
- **Phase 2 — policy resources** (trigger: ready after Phase 1): host store,
  `use` chains, `no use = use default`, `@@` exceptions, `clawk policy` CLI,
  live propagation via reload. Namespace network fields become references
  resolved at up/reload instead of copies flattened at create
  (`applyNamespaceDefaults` staleness fix).
- **Phase 3 — grammar unification** (trigger: real demand for policies
  defined in repo files, or two-filename friction; **do not build on spec**):
  typed blocks, `sandbox`/`policy`/`namespace`, single filename, hard
  cutover, rolling spec, multi-document + directory apply.
- **Phase 4 — deferred futures** (compatible, not designed-for): SNI name
  enforcement; multiple sandbox blocks per clawk.mod (variants/fleets — would
  need default-selection, `--as` selector, (dir, block) anchors); template
  inheritance (Pkl `amends` / Compose `extends`); nested `includes`;
  `clawk.d/` sharding; explicit K8s-style `Pass` verdict; per-port rules
  (rejected: every proxy-based peer is name-only, and the threat model is
  destinations, not ports).

Each phase ships independently; syntax lands last, on proven semantics,
before an installed base accumulates — the reverse of the HCL1/WORKSPACE
sequence that forced those ecosystems' expensive do-overs.

---

## Prior art consulted

- Kubernetes ClusterNetworkPolicy v1alpha2 / AdminNetworkPolicy tiers & Pass:
  network-policy-api.sigs.k8s.io (blog 2025-10-09, 2024-01-30)
- Cilium DNS-based policies & limitations: docs.cilium.io/en/stable/security/dns;
  cilium/cilium#31803
- Cedar order-independence & forbid-overrides-permit: AWS Security Blog,
  "How we designed Cedar"
- Little Snitch rule precedence: help.obdev.at/littlesnitch5/ref-rule-precedence
- Pi-hole whitelist precedence: docs.pi-hole.net/guides/misc/whitelist-blacklist
- uBlock/Adblock exception rules; OISD "no breakage" curation
- Anthropic sandbox-runtime (proxy allow/deny, interactive approval):
  github.com/anthropic-experimental/sandbox-runtime
- HCL attributes-as-blocks compat shim: developer.hashicorp.com/terraform/language/attr-as-blocks
- Bazel WORKSPACE→bzlmod migration: bazel.build/external/migration
- Docker Compose: version-field removal, `include` semantics, project model:
  docs.docker.com (compose-file reference, compose history)
- CUE (commutative unification), Pkl (`amends`), Nomad (parameterized job
  dispatch), Fly.io (app/machines) — instance-naming and merge-semantics
  precedents

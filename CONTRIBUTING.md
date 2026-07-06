# Contributing to clawk

Thanks for your interest. clawk is pre-1.0 and moving quickly — issues and PRs
are welcome.

## Before you start

- Skim [ARCHITECTURE.md](ARCHITECTURE.md) for how the codebase fits together
  (the detached daemon, the guest stack, networking, the `machine` module, and
  the package map). [DESIGN.md](DESIGN.md) goes deeper into each subsystem and
  the decisions behind it.
- For anything non-trivial, open an issue to discuss the approach before
  writing a lot of code.

## Project layout

Two Go modules:

- the root module (`github.com/clawkwork/clawk`) — CLI, providers, guest setup;
- `machine/` (`github.com/clawkwork/clawk/machine`) — the hypervisor-neutral VM
  library, wired in with a `replace` directive.

Build, vet, and test each separately (`machine/` isn't covered by the root
`./...`).

## Build & test

```sh
make build        # build ./bin/clawk (codesigns on macOS)
make install      # build + install + codesign into $GOBIN
make test         # go test ./...
go vet ./...      # vet-clean
```

The `machine/` module is separate, so build/test/vet it on its own too:

```sh
cd machine && go build ./... && go test ./...
make -C machine smoke-firecracker   # boot a microVM end-to-end (Linux + /dev/kvm)
make -C machine smoke-alpine        # boot an Alpine image (macOS, codesigned)
```

The end-to-end VM boot tests (`internal/e2e`, `machine/cmd/smoke-*`) are opt-in
and need a real hypervisor — run them locally when you touch the boot path.

## Platform constraints

The two VM providers are mutually exclusive at the build level:

- **vz** (`*_darwin.go`, `machine/vz`) compiles only on macOS — it's cgo +
  Apple Virtualization.framework, and the binary must be codesigned
  (`make build` / `make install` do this).
- **firecracker** (`*_linux.go`, `machine/firecracker`) compiles only on Linux
  and needs the `firecracker` binary plus `/dev/kvm` to actually boot.

Most logic is shared and platform-independent; only the hypervisor glue is
`//go:build`-gated. So a change to shared code can be developed on either OS,
but a change to a provider needs that provider's OS to compile. Please note in
your PR which platform(s) you built and tested on.

## Before you open a PR

Run, from the repo root and from `machine/`:

```sh
gofmt -l .              # must print nothing
go vet ./...
go test ./...
```

CI runs the same on Linux and macOS (see `.github/workflows/ci.yml`); PRs must
be green.

## Extending

- **New coding-agent runner** — add one entry to the runner registry in
  `internal/cli`; dispatch, args, and lifecycle are shared.
- **New VM provider** — implement `sandbox.Provider` (plus the optional
  `ShellProvider` / `CaptureExecProvider` / … capability interfaces) and
  register a `machine.Backend`; wire it into the per-OS `newProvider`.

## Debugging

Set `CLAWK_DEBUG=1` for verbose daemon logging. Per-sandbox artifacts live
under `~/.clawk/namespaces/<ns>/vms/<name>/` (`console.log` for kernel
output, `vzd.log` / `fcd.log` for the daemon). `clawk debug dump <name>` collects a
postmortem bundle; `clawk debug vshell <name>` is a raw vsock shell that
exercises the agent path in isolation.

## Style

- Match the surrounding code; keep the comment density and naming idiom of the
  file you're editing.
- Keep PRs focused — one logical change per PR is much easier to review.
- Write commit messages that explain the *why*, not just the *what*.

## License

By contributing, you agree that your contributions are licensed under the
project's [Apache License 2.0](LICENSE).

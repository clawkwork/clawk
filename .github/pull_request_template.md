## What / why

<!-- What does this change do, and why? -->

## Platform(s) built and tested on

<!-- vz needs macOS, firecracker needs Linux — the providers are build-gated,
so note which one(s) you actually exercised. -->

- [ ] macOS (vz)
- [ ] Linux (firecracker)

## Checklist

- [ ] `gofmt -l .` is clean
- [ ] `go build ./... && go vet ./... && go test ./...` pass from the repo root
- [ ] `go build ./... && go vet ./... && go test ./...` pass from `machine/`
- [ ] One logical change per PR
- [ ] Commit messages explain the *why*, not just the *what*
- [ ] No Co-Authored-By / attribution trailers

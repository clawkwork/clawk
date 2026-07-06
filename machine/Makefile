ENTITLEMENTS := $(abspath ../clawk.entitlements)
BIN_DIR := $(CURDIR)/bin

# smoke-alpine boots Alpine via the vz backend end-to-end. Requires codesigning
# with com.apple.security.virtualization, so it can't be run via `go run`.
.PHONY: smoke-alpine
smoke-alpine:
	@mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/smoke-alpine ./cmd/smoke-alpine
	codesign --entitlements $(ENTITLEMENTS) -s - --force $(BIN_DIR)/smoke-alpine
	$(BIN_DIR)/smoke-alpine $(ARGS)

# smoke-firecracker boots Ubuntu via the firecracker backend. Linux-only —
# meant to be run inside a nested clawk guest (or on bare-metal Linux with
# /dev/kvm). No codesign step needed.
.PHONY: smoke-firecracker
smoke-firecracker:
	@mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/smoke-firecracker ./cmd/smoke-firecracker
	$(BIN_DIR)/smoke-firecracker $(ARGS)

.PHONY: clean
clean:
	rm -rf $(BIN_DIR)

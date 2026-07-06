package cli

import (
	"encoding/base64"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/sandbox"
)

// pushHostFiles copies every entry in sb.Files from host to guest. Runs
// on every `clawk up`, so users editing the host file pick up the
// change with one command — no destroy required.
//
// Implementation: base64-encode the file content and inline it into a
// single `bash -c` invocation through ShellProvider.Exec. Cheap and
// stdin-free, suitable for credentials and small config files (the only
// use case the directive exists for). Files larger than pushFileMaxBytes
// are rejected before we attempt to push them so a 200MB log file in the
// clawk.mod doesn't silently blow out a kernel arg-limit ceiling.
//
// Errors are reported but non-fatal: a missing host file or a transient
// push glitch should not block `clawk up` (the runner can still start;
// the user is warned and can re-run `up` after fixing the source).
func pushHostFiles(provider sandbox.Provider, sb *config.Sandbox) error {
	if len(sb.Files) == 0 {
		return nil
	}
	sp, ok := provider.(sandbox.ShellProvider)
	if !ok {
		return nil
	}
	var firstErr error
	for _, f := range sb.Files {
		if err := pushOneHostFile(sp, sb, f); err != nil {
			fmt.Fprintf(os.Stderr, "warning: pushing %s: %v\n", f.GuestPath, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// pushFileMaxBytes caps a single inlined push at 1 MiB of raw content
// (~1.4 MiB base64). Plenty of headroom for any kube config, AWS
// credentials, .netrc, or Docker config; safely below the kernel
// ARG_MAX on macOS/Linux even with command-line overhead.
const pushFileMaxBytes = 1 << 20

func pushOneHostFile(sp sandbox.ShellProvider, sb *config.Sandbox, f config.HostFile) error {
	info, err := os.Stat(f.HostPath)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory", f.HostPath)
	}
	if info.Size() > pushFileMaxBytes {
		return fmt.Errorf("%s is %d bytes; max %d for inline push (use 'shares (...)' for large files)",
			f.HostPath, info.Size(), pushFileMaxBytes)
	}
	data, err := os.ReadFile(f.HostPath)
	if err != nil {
		return err
	}
	mode := f.Mode
	if mode == 0 {
		mode = uint32(info.Mode().Perm())
	}
	script := buildFilePushScript(f.GuestPath, data, fs.FileMode(mode))
	return sp.Exec(sb, "bash", "-c", script)
}

// buildFilePushScript renders the in-guest shell command that
// reconstitutes one host file at the given guest path with the given
// mode. Owned root:root and chowned to agent afterwards so an agent
// reading credentials doesn't need sudo. `install` is used for atomic
// publish (write tmpfile in target dir, rename onto destination) so a
// concurrent reader never sees a partial file.
func buildFilePushScript(guestPath string, data []byte, mode fs.FileMode) string {
	encoded := base64.StdEncoding.EncodeToString(data)
	// Use printf %s to dodge any shell interpretation; pipe into
	// `base64 -d` (busybox + coreutils both accept it) then `install`
	// which handles the mkdir-of-parent, mode, and ownership in one step.
	return strings.Join([]string{
		"set -euo pipefail",
		"dir=" + pushShellQuote(parentDir(guestPath)),
		"sudo mkdir -p \"$dir\"",
		fmt.Sprintf(
			"printf %%s %s | base64 -d | sudo install -o %s -g %s -m %#o /dev/stdin %s",
			pushShellQuote(encoded),
			sandbox.GuestUser, sandbox.GuestUser, mode.Perm(),
			pushShellQuote(guestPath),
		),
	}, " && ")
}

// parentDir returns the directory component of a guest path. We avoid
// path/filepath here because that package follows host (macOS)
// conventions; the script runs in a Linux guest where `/` is the only
// separator regardless of host OS.
func parentDir(p string) string {
	if idx := strings.LastIndex(p, "/"); idx > 0 {
		return p[:idx]
	}
	return "/"
}

// pushShellQuote wraps s in single quotes for safe interpolation into a
// bash -c script. Mirrors sandbox.shellQuote but kept private to this
// package so we don't widen sandbox's exported API just for one helper.
func pushShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

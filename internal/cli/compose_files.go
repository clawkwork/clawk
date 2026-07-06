package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/sandbox"
	"github.com/clawkwork/clawk/internal/template"
)

// mergeFiles gathers `files (...)` entries from the workspace and every
// repo Clawkfile, resolves each one to absolute (host, guest) paths, and
// rejects duplicate guest paths with a message naming both sources.
//
// Two repos legitimately wanting the same host file at different guest
// paths is fine; two repos disagreeing on what to put at the same guest
// path is a config bug we won't silently dedupe.
func mergeFiles(ws *template.Workspace) ([]config.HostFile, error) {
	var sources []fileSource
	for _, f := range ws.File.Files {
		sources = append(sources, fileSource{Origin: "workspace", Spec: f})
	}
	for _, r := range ws.Repos {
		if r.Clawkfile == nil {
			continue
		}
		for _, f := range r.Clawkfile.Files {
			sources = append(sources, fileSource{Origin: r.Name, Spec: f})
		}
	}
	return composeFiles(sources)
}

// mergeShares is the share-block analogue of mergeFiles. Same conflict
// rule: same guest mount point declared with diverging options is a
// config error, identical specs from multiple sources are collapsed.
func mergeShares(ws *template.Workspace) ([]config.HostShare, error) {
	var sources []shareSource
	for _, s := range ws.File.Shares {
		sources = append(sources, shareSource{Origin: "workspace", Spec: s})
	}
	for _, r := range ws.Repos {
		if r.Clawkfile == nil {
			continue
		}
		for _, s := range r.Clawkfile.Shares {
			sources = append(sources, shareSource{Origin: r.Name, Spec: s})
		}
	}
	return composeShares(sources)
}

type fileSource struct {
	Origin string
	Spec   template.FileSpec
}

type shareSource struct {
	Origin string
	Spec   template.ShareSpec
}

// composeFiles is the pure conflict-detection core split out from
// mergeFiles so the here-mode path (single Clawkfile) and the workspace
// path can share it.
func composeFiles(sources []fileSource) ([]config.HostFile, error) {
	byGuest := make(map[string]struct {
		Origin string
		Host   string
	})
	var out []config.HostFile
	for _, s := range sources {
		host, guest, err := resolveFilePaths(s.Spec.HostPath, s.Spec.GuestPath)
		if err != nil {
			return nil, fmt.Errorf("%s file %q: %w", s.Origin, s.Spec.HostPath, err)
		}
		entry := config.HostFile{
			HostPath:  host,
			GuestPath: guest,
			Mode:      uint32(s.Spec.Mode),
		}
		if prev, dup := byGuest[guest]; dup {
			if prev.Host == host {
				continue
			}
			return nil, fmt.Errorf(
				"guest file %s declared by both %s (%s) and %s (%s) — change one",
				guest, prev.Origin, prev.Host, s.Origin, host)
		}
		byGuest[guest] = struct {
			Origin string
			Host   string
		}{s.Origin, host}
		out = append(out, entry)
	}
	return out, nil
}

func composeShares(sources []shareSource) ([]config.HostShare, error) {
	byGuest := make(map[string]struct {
		Origin   string
		Host     string
		ReadOnly bool
	})
	var out []config.HostShare
	for _, s := range sources {
		host, guest, err := resolveSharePaths(s.Spec.HostPath, s.Spec.GuestPath)
		if err != nil {
			return nil, fmt.Errorf("%s share %q: %w", s.Origin, s.Spec.HostPath, err)
		}
		entry := config.HostShare{
			HostPath:  host,
			GuestPath: guest,
			ReadOnly:  s.Spec.ReadOnly,
		}
		if prev, dup := byGuest[guest]; dup {
			if prev.Host == host && prev.ReadOnly == s.Spec.ReadOnly {
				continue
			}
			return nil, fmt.Errorf(
				"guest mount %s declared by both %s (%s) and %s (%s) — change one",
				guest, prev.Origin, prev.Host, s.Origin, host)
		}
		byGuest[guest] = struct {
			Origin   string
			Host     string
			ReadOnly bool
		}{s.Origin, host, s.Spec.ReadOnly}
		out = append(out, entry)
	}
	return out, nil
}

// resolveFilePaths normalises one (host, guest) pair into absolute paths.
//
// Host side: ~ is expanded against the current host's $HOME. The host
// path must exist and be a regular file — we read it at `clawk up` time
// to push into the VM, so catching the missing-file case at compose time
// gives a much better error than a vague push failure later.
//
// Guest side: empty defaults to mirroring the host path with the host's
// $HOME prefix swapped for the guest's $HOME, so `~/.kube/cfg` on a Mac
// becomes `/home/agent/.kube/cfg` inside the VM with no explicit guest
// path. Explicit guest paths must be absolute.
func resolveFilePaths(rawHost, rawGuest string) (host, guest string, _ error) {
	host, err := expandHostPath(rawHost)
	if err != nil {
		return "", "", err
	}
	fi, err := os.Stat(host)
	if err != nil {
		return "", "", fmt.Errorf("stat %s: %w", host, err)
	}
	if fi.IsDir() {
		return "", "", fmt.Errorf("%s is a directory; use 'shares (...)' for live directory mounts", host)
	}
	guest, err = resolveGuestPath(rawHost, rawGuest)
	if err != nil {
		return "", "", err
	}
	return host, guest, nil
}

// resolveSharePaths is the directory-flavoured sibling of
// resolveFilePaths. Same expansion + guest-default rules, but the host
// path must be an existing directory (virtio-fs only mounts dirs).
func resolveSharePaths(rawHost, rawGuest string) (host, guest string, _ error) {
	host, err := expandHostPath(rawHost)
	if err != nil {
		return "", "", err
	}
	fi, err := os.Stat(host)
	if err != nil {
		return "", "", fmt.Errorf("stat %s: %w", host, err)
	}
	if !fi.IsDir() {
		return "", "", fmt.Errorf("%s is not a directory; use 'files (...)' for individual files", host)
	}
	guest, err = resolveGuestPath(rawHost, rawGuest)
	if err != nil {
		return "", "", err
	}
	return host, guest, nil
}

// expandHostPath turns a clawk.mod path into an absolute host path. `~`
// and `~/` are expanded against the user's home directory; relative
// paths are made absolute against the current working directory.
//
// We deliberately do not expand `$VAR` substitutions — clawk.mod is meant
// to be reproducible across machines, and env-var interpolation invites
// "works on my laptop" config drift. The one exception is the `~` prefix
// which every user expects.
func expandHostPath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	if p == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return home, nil
	}
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, p[2:])
	}
	if !filepath.IsAbs(p) {
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", err
		}
		p = abs
	}
	return filepath.Clean(p), nil
}

// resolveGuestPath turns the raw guest path (which may be empty,
// `~`-prefixed, or absolute) into an absolute guest-side path. An empty
// guest path mirrors the raw host path under the agent's home directory
// inside the VM, so `~/.aws` on the host lands at `/home/agent/.aws` in
// the guest without the user having to repeat themselves.
func resolveGuestPath(rawHost, rawGuest string) (string, error) {
	if rawGuest == "" {
		return guestMirrorOfHost(rawHost)
	}
	if rawGuest == "~" {
		return sandbox.GuestHome, nil
	}
	if strings.HasPrefix(rawGuest, "~/") {
		return filepath.Join(sandbox.GuestHome, rawGuest[2:]), nil
	}
	if !filepath.IsAbs(rawGuest) {
		return "", fmt.Errorf("guest path %q must be absolute or start with '~'", rawGuest)
	}
	return filepath.Clean(rawGuest), nil
}

// guestMirrorOfHost is the empty-guest default: take the raw host path
// (still in clawk.mod form, before host-home expansion) and rebase it on
// the guest's home directory. We work from the raw form so a Mac user
// who wrote `~/.kube/cfg` doesn't end up with `/Users/foo/.kube/cfg`
// inside a Linux VM — the `~` was the user's intent, not the absolute
// `/Users/foo` it resolved to.
func guestMirrorOfHost(rawHost string) (string, error) {
	if rawHost == "~" {
		return sandbox.GuestHome, nil
	}
	if strings.HasPrefix(rawHost, "~/") {
		return filepath.Join(sandbox.GuestHome, rawHost[2:]), nil
	}
	if filepath.IsAbs(rawHost) {
		// Absolute host path with no guest override: try to remap the
		// user's host $HOME prefix onto the guest $HOME, otherwise mount
		// at the same absolute path on both sides.
		home, err := os.UserHomeDir()
		if err == nil && strings.HasPrefix(rawHost, home+string(filepath.Separator)) {
			rel := strings.TrimPrefix(rawHost, home+string(filepath.Separator))
			return filepath.Join(sandbox.GuestHome, rel), nil
		}
		return filepath.Clean(rawHost), nil
	}
	return "", fmt.Errorf("cannot infer guest path from relative host %q; declare an explicit guest path", rawHost)
}

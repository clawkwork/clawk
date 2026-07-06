// Package ext4 converts a tar stream into a mountable ext4 disk image
// entirely in user space — no root, no loop devices, no e2fsprogs.
//
// The input is expected to be a flattened filesystem tar (e.g. the output
// of go-containerregistry's mutate.Extract): OCI whiteout entries are
// rejected rather than interpreted. Ownership, permissions (including
// setuid/setgid), xattrs, device nodes and hardlinks are preserved, which
// an unprivileged untar-to-directory cannot do.
//
// Forked from github.com/Microsoft/hcsshim/ext4/tar2ext4 (MIT); see
// README.md and LICENSE in this directory for provenance and the list of
// local changes.
package ext4

import (
	"archive/tar"
	"bufio"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/clawkwork/clawk/machine/internal/ext4/internal/compactext4"
)

// ConvertOption configures a Convert call.
type ConvertOption func(*convertParams)

type convertParams struct {
	ext4opts []compactext4.Option
}

// Writable produces a filesystem the kernel will mount read-write. Without
// it the image carries RO_COMPAT_READONLY and rw mounts are refused.
func Writable() ConvertOption {
	return func(p *convertParams) {
		p.ext4opts = append(p.ext4opts, compactext4.Writable)
	}
}

// TotalSize pads the filesystem to at least size bytes so the guest has
// free space to allocate from. Content larger than size wins. The padding
// is materialized as a sparse hole when the output supports Truncate, so
// it costs no physical disk.
func TotalSize(size int64) ConvertOption {
	return func(p *convertParams) {
		p.ext4opts = append(p.ext4opts, compactext4.TotalDiskSize(size))
	}
}

// Convert reads a flattened filesystem tar from r and writes an ext4 image
// to w. Entries may arrive in any order (parents are created on demand and
// later refined when their real entry arrives); hardlinks whose target
// appears later in the stream are deferred, and hardlinks whose target
// never appears are dropped, matching common unpackers.
func Convert(r io.Reader, w io.ReadWriteSeeker, opts ...ConvertOption) error {
	var p convertParams
	for _, opt := range opts {
		opt(&p)
	}

	type hardlink struct {
		target, name string
	}
	var deferred []hardlink

	t := tar.NewReader(bufio.NewReader(r))
	fs := compactext4.NewWriter(w, p.ext4opts...)
	for {
		hdr, err := t.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("ext4: reading tar: %w", err)
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			// PAX global headers (e.g. BuildKit's pax_global_header)
			// carry no filesystem content.
			continue
		}
		name, ok := normalize(hdr.Name)
		if !ok {
			continue
		}
		if strings.HasPrefix(path.Base(name), ".wh.") {
			return fmt.Errorf("ext4: whiteout entry %q: input must be a flattened tar", hdr.Name)
		}

		if err := fs.MakeParents(name); err != nil {
			return fmt.Errorf("ext4: parents of %q: %w", name, err)
		}

		if hdr.Typeflag == tar.TypeLink {
			target, ok := normalize(hdr.Linkname)
			if !ok {
				continue
			}
			if _, err := fs.Stat(target); err != nil {
				// Flattened tars walk layers top-down, so a link can
				// precede its target. Retry once the stream is done.
				deferred = append(deferred, hardlink{target: target, name: name})
				continue
			}
			if err := fs.Link(target, name); err != nil {
				return fmt.Errorf("ext4: hardlink %q -> %q: %w", name, target, err)
			}
			continue
		}

		f := &compactext4.File{
			Mode:     uint16(hdr.Mode),
			Atime:    hdr.AccessTime,
			Mtime:    hdr.ModTime,
			Ctime:    hdr.ChangeTime,
			Crtime:   hdr.ModTime,
			Size:     hdr.Size,
			Uid:      uint32(hdr.Uid),
			Gid:      uint32(hdr.Gid),
			Linkname: hdr.Linkname,
			Devmajor: uint32(hdr.Devmajor),
			Devminor: uint32(hdr.Devminor),
			Xattrs:   make(map[string][]byte),
		}
		for key, value := range hdr.PAXRecords {
			const xattrPrefix = "SCHILY.xattr."
			if strings.HasPrefix(key, xattrPrefix) {
				f.Xattrs[key[len(xattrPrefix):]] = []byte(value)
			}
		}

		var typ uint16
		switch hdr.Typeflag {
		case tar.TypeReg:
			typ = compactext4.S_IFREG
		case tar.TypeSymlink:
			typ = compactext4.S_IFLNK
		case tar.TypeChar:
			typ = compactext4.S_IFCHR
		case tar.TypeBlock:
			typ = compactext4.S_IFBLK
		case tar.TypeDir:
			typ = compactext4.S_IFDIR
		case tar.TypeFifo:
			typ = compactext4.S_IFIFO
		default:
			// Unknown entry kinds (GNU sparse, vendor extensions) would
			// otherwise materialize as empty regular files; skip them.
			continue
		}
		f.Mode &= ^compactext4.TypeMask
		f.Mode |= typ
		if err := fs.Create(name, f); err != nil {
			return fmt.Errorf("ext4: creating %q: %w", name, err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := io.Copy(fs, t); err != nil {
				return fmt.Errorf("ext4: writing %q: %w", name, err)
			}
		}
	}

	for _, l := range deferred {
		if _, err := fs.Stat(l.target); err != nil {
			continue // dangling hardlink in the image; drop it
		}
		if err := fs.Link(l.target, l.name); err != nil {
			return fmt.Errorf("ext4: hardlink %q -> %q: %w", l.name, l.target, err)
		}
	}

	if err := fs.Close(); err != nil {
		return fmt.Errorf("ext4: finalizing filesystem: %w", err)
	}
	return nil
}

// normalize cleans a tar entry path into the rootfs-relative form
// compactext4 expects. The boolean is false for entries that name the
// root itself or escape it.
func normalize(name string) (string, bool) {
	name = strings.TrimPrefix(name, "/")
	name = path.Clean(name)
	if name == "." || name == ".." || strings.HasPrefix(name, "../") || name == "" {
		return "", false
	}
	return name, true
}

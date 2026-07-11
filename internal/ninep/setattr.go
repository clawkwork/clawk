package ninep

import (
	"os"
	"path"

	"github.com/hugelgupf/p9/p9"
)

// localfs.SetAttr only honors size and timestamps and returns ENOSYS for
// everything else — including permission and ownership changes. That breaks
// any workload that chmods a file on the share: a Go toolchain unpacked into a
// 9p-mounted GOMODCACHE never gets its exec bit (chmod(2) fails with ENOSYS),
// so `go` can't run the freshly downloaded toolchain and the build dies. The
// cargo caches hit the same wall for any tool that adjusts modes.
//
// chmodAttacher wraps localfs so SetAttr applies mode/owner changes against the
// backing file directly, delegating every other operation unchanged. The
// wrapper tracks each fid's host path (mirroring localfs's own bookkeeping) so
// it can chmod the right file, and unwraps File arguments before handing them
// back to localfs, whose Link/RenameAt/Renamed type-assert to their concrete
// *Local — a raw wrapper would panic that assertion the moment the guest
// renames a file (which the toolchain unpack does constantly).
type chmodAttacher struct {
	inner p9.Attacher
	root  string
}

func (a chmodAttacher) Attach() (p9.File, error) {
	f, err := a.inner.Attach()
	if err != nil {
		return nil, err
	}
	return &chmodFile{File: f, path: a.root}, nil
}

// chmodFile decorates a localfs file with a working SetAttr. It embeds the
// p9.File interface so every method it does not override falls through to
// localfs; the ones it does override either fix SetAttr or keep the tracked
// path correct across the operations that mint or move fids.
type chmodFile struct {
	p9.File
	path string
}

// unwrap returns the localfs file a chmodFile decorates, so localfs methods
// that type-assert their File arguments to *Local receive the concrete type.
func unwrap(f p9.File) p9.File {
	if w, ok := f.(*chmodFile); ok {
		return w.File
	}
	return f
}

func (f *chmodFile) Walk(names []string) ([]p9.QID, p9.File, error) {
	qids, child, err := f.File.Walk(names)
	if err != nil {
		return qids, child, err
	}
	// Walk with no names clones the fid in place; with names it descends.
	p := f.path
	for _, name := range names {
		p = path.Join(p, name)
	}
	return qids, &chmodFile{File: child, path: p}, nil
}

func (f *chmodFile) Create(name string, mode p9.OpenFlags, perm p9.FileMode, uid p9.UID, gid p9.GID) (p9.File, p9.QID, uint32, error) {
	child, qid, iounit, err := f.File.Create(name, mode, perm, uid, gid)
	if err != nil {
		return child, qid, iounit, err
	}
	return &chmodFile{File: child, path: path.Join(f.path, name)}, qid, iounit, nil
}

func (f *chmodFile) Link(target p9.File, newName string) error {
	return f.File.Link(unwrap(target), newName)
}

func (f *chmodFile) RenameAt(oldName string, newDir p9.File, newName string) error {
	return f.File.RenameAt(oldName, unwrap(newDir), newName)
}

func (f *chmodFile) Renamed(newDir p9.File, newName string) {
	f.File.Renamed(unwrap(newDir), newName)
	if w, ok := newDir.(*chmodFile); ok {
		f.path = path.Join(w.path, newName)
	}
}

// SetAttr applies the fields localfs rejects. Timestamps are intentionally
// accepted-and-ignored, matching localfs's prior behavior (a truncate(2)
// carries an mtime the client does not actually depend on the server to honor);
// the point of this override is chmod/chown, which localfs never implemented.
func (f *chmodFile) SetAttr(valid p9.SetAttrMask, attr p9.SetAttr) error {
	if valid.Permissions {
		if err := os.Chmod(f.path, osMode(attr.Permissions)); err != nil {
			return err
		}
	}
	if valid.UID || valid.GID {
		uid, gid := -1, -1
		if valid.UID {
			uid = int(attr.UID)
		}
		if valid.GID {
			gid = int(attr.GID)
		}
		if err := os.Lchown(f.path, uid, gid); err != nil {
			return err
		}
	}
	if valid.Size {
		if err := os.Truncate(f.path, int64(attr.Size)); err != nil {
			return err
		}
	}
	return nil
}

// osMode maps a 9p permission mode to an os.FileMode, carrying the rwx and
// sticky bits (the range p9.FileMode.Permissions preserves).
func osMode(m p9.FileMode) os.FileMode {
	perm := m.Permissions()
	mode := os.FileMode(perm & 0o777)
	if perm&0o1000 != 0 {
		mode |= os.ModeSticky
	}
	return mode
}

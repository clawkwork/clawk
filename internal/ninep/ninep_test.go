package ninep

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/hugelgupf/p9/p9"
)

// TestServeReadWriteRoundTrip drives the server the way a guest would: attach,
// read a file the host already wrote, then create+write a file and confirm it
// lands on the host directory. The write-back is the property that keeps the
// cache shared across sandboxes (a module fetched in one VM is on disk for the
// next), so it's the thing worth asserting.
func TestServeReadWriteRoundTrip(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("from-host"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	sock := tempSock(t)
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	go func() { _ = s.Serve(l) }()

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	client, err := p9.NewClient(conn)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	root9, err := client.Attach("")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer root9.Close()

	// Guest reads a host-written file.
	_, hello, err := root9.Walk([]string{"hello.txt"})
	if err != nil {
		t.Fatalf("walk hello.txt: %v", err)
	}
	if _, _, err := hello.Open(p9.ReadOnly); err != nil {
		t.Fatalf("open hello.txt: %v", err)
	}
	buf := make([]byte, 64)
	n, err := hello.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("read hello.txt: %v", err)
	}
	if got := string(buf[:n]); got != "from-host" {
		t.Fatalf("read = %q, want %q", got, "from-host")
	}
	_ = hello.Close()

	// Guest creates+writes a file; it must appear on the host dir (write-back).
	_, dir, err := root9.Walk(nil) // clone the root fid; Create consumes it
	if err != nil {
		t.Fatalf("walk root clone: %v", err)
	}
	created, _, _, err := dir.Create("world.txt", p9.ReadWrite, 0o644, p9.UID(os.Getuid()), p9.GID(os.Getgid()))
	if err != nil {
		t.Fatalf("create world.txt: %v", err)
	}
	if _, err := created.WriteAt([]byte("from-guest"), 0); err != nil {
		t.Fatalf("write world.txt: %v", err)
	}
	_ = created.Close()

	got, err := os.ReadFile(filepath.Join(root, "world.txt"))
	if err != nil {
		t.Fatalf("host read-back: %v", err)
	}
	if string(got) != "from-guest" {
		t.Fatalf("host file = %q, want %q", got, "from-guest")
	}
}

// TestSetAttrChmod is the regression test for the toolchain-exec-bit bug:
// localfs.SetAttr ENOSYS-es any permission change, so a Go toolchain unpacked
// into a 9p-mounted GOMODCACHE never becomes executable. The server must apply
// the chmod to the backing file. Driven through the real p9 client so it
// exercises the same SetAttr path the guest kernel's 9p client does.
func TestSetAttrChmod(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "tool"), []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	root9 := attachTestClient(t, root)

	_, tool, err := root9.Walk([]string{"tool"})
	if err != nil {
		t.Fatalf("walk tool: %v", err)
	}
	if err := tool.SetAttr(p9.SetAttrMask{Permissions: true}, p9.SetAttr{Permissions: 0o755}); err != nil {
		t.Fatalf("chmod tool: %v", err)
	}

	fi, err := os.Stat(filepath.Join(root, "tool"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o755 {
		t.Fatalf("host mode = %o, want %o", got, 0o755)
	}
}

// TestSetAttrChmodOnCreatedAndRenamed covers the fid-minting/moving paths the
// wrapper has to keep straight: a chmod on a fid obtained from Create, and a
// RenameAt (whose localfs implementation type-asserts its File argument to
// *Local — a naive wrapper panics here).
func TestSetAttrChmodOnCreatedAndRenamed(t *testing.T) {
	root := t.TempDir()
	root9 := attachTestClient(t, root)

	_, dir, err := root9.Walk(nil) // Create consumes the fid; clone first
	if err != nil {
		t.Fatalf("walk clone: %v", err)
	}
	created, _, _, err := dir.Create("fresh", p9.WriteOnly, 0o644, p9.UID(os.Getuid()), p9.GID(os.Getgid()))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := created.SetAttr(p9.SetAttrMask{Permissions: true}, p9.SetAttr{Permissions: 0o750}); err != nil {
		t.Fatalf("chmod created: %v", err)
	}
	_ = created.Close()
	if fi, err := os.Stat(filepath.Join(root, "fresh")); err != nil {
		t.Fatal(err)
	} else if got := fi.Mode().Perm(); got != 0o750 {
		t.Fatalf("created mode = %o, want %o", got, 0o750)
	}

	// RenameAt within the root: must not panic on the *Local assertion.
	_, rdir, err := root9.Walk(nil)
	if err != nil {
		t.Fatalf("walk clone for rename: %v", err)
	}
	if err := rdir.RenameAt("fresh", unwrapDir(t, root9), "moved"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "moved")); err != nil {
		t.Fatalf("renamed file missing: %v", err)
	}
}

// attachTestClient serves root over a unix socket and returns an attached p9
// client root, cleaning both up when the test ends.
// tempSock returns a unix-socket path short enough for macOS's ~104-byte
// sun_path limit. t.TempDir() embeds the (long) test name and can overflow it,
// surfacing as "bind: invalid argument" on darwin.
func tempSock(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "9p")
	if err != nil {
		t.Fatalf("mkdir temp sock: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

func attachTestClient(t *testing.T, root string) p9.File {
	t.Helper()
	s, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sock := tempSock(t)
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() { _ = s.Serve(l) }()

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	client, err := p9.NewClient(conn)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	root9, err := client.Attach("")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = root9.Close() })
	return root9
}

// unwrapDir returns a fresh clone of the root fid to serve as RenameAt's target
// directory (RenameAt consumes neither fid but needs a live handle to newDir).
func unwrapDir(t *testing.T, root9 p9.File) p9.File {
	t.Helper()
	_, d, err := root9.Walk(nil)
	if err != nil {
		t.Fatalf("walk newdir: %v", err)
	}
	return d
}

// TestNewRejectsBadRoot keeps New honest: a missing or non-directory root is a
// caller bug (the cache dir is created before serving), not something to serve
// anyway.
func TestNewRejectsBadRoot(t *testing.T) {
	if _, err := New(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("New(missing dir) = nil error, want error")
	}
	f := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(f); err == nil {
		t.Fatal("New(file) = nil error, want error")
	}
}

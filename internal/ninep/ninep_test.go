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

	sock := filepath.Join(t.TempDir(), "9p.sock")
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

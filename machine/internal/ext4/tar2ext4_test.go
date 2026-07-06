package ext4

import (
	"archive/tar"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type tarEntry struct {
	name     string
	typeflag byte
	mode     int64
	uid, gid int
	body     string
	link     string
	devmajor int64
	devminor int64
	xattrs   map[string]string
}

func buildTar(t *testing.T, entries []tarEntry) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: e.typeflag,
			Mode:     e.mode,
			Uid:      e.uid,
			Gid:      e.gid,
			Size:     int64(len(e.body)),
			Linkname: e.link,
			Devmajor: e.devmajor,
			Devminor: e.devminor,
		}
		for k, v := range e.xattrs {
			if hdr.PAXRecords == nil {
				hdr.PAXRecords = map[string]string{}
			}
			hdr.PAXRecords["SCHILY.xattr."+k] = v
		}
		err := tw.WriteHeader(hdr)
		require.NoError(t, err, "writing header %q", e.name)
		if e.body != "" {
			_, err = tw.Write([]byte(e.body))
			require.NoError(t, err, "writing body %q", e.name)
		}
	}
	err := tw.Close()
	require.NoError(t, err)
	return &buf
}

func convertToImage(t *testing.T, tarBuf *bytes.Buffer, opts ...ConvertOption) string {
	t.Helper()
	image := filepath.Join(t.TempDir(), "fs.img")
	f, err := os.Create(image)
	require.NoError(t, err)
	defer f.Close()
	err = Convert(tarBuf, f, opts...)
	require.NoError(t, err, "Convert")
	return image
}

// debugfsStat runs `debugfs -R "stat <path>"` and returns its output.
// Skips the calling test when debugfs isn't installed.
func debugfsStat(t *testing.T, image, path string) string {
	t.Helper()
	return debugfsCmd(t, image, "stat "+path)
}

func debugfsCmd(t *testing.T, image, cmd string) string {
	t.Helper()
	debugfs, err := exec.LookPath("debugfs")
	if err != nil {
		t.Skip("debugfs not installed")
	}
	out, err := exec.Command(debugfs, "-R", cmd, image).CombinedOutput()
	if err != nil {
		t.Fatalf("debugfs -R %q: %v\n%s", cmd, err, out)
	}
	return string(out)
}

func e2fsckClean(t *testing.T, image string) {
	t.Helper()
	e2fsck, err := exec.LookPath("e2fsck")
	if err != nil {
		t.Skip("e2fsck not installed")
	}
	if out, err := exec.Command(e2fsck, "-f", "-n", image).CombinedOutput(); err != nil {
		t.Errorf("e2fsck: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}

func TestConvert(t *testing.T) {
	image := convertToImage(t, buildTar(t, []tarEntry{
		// PAX global header noise, as emitted by BuildKit.
		{name: "pax_global_header", typeflag: tar.TypeXGlobalHeader},
		// Root entry — refers to the rootfs itself, must be skipped.
		{name: "./", typeflag: tar.TypeDir, mode: 0o755},
		// Out-of-order: child before its parent's own entry; the parent
		// entry arrives later with a tighter mode and must win.
		{name: "./a/b/c.txt", typeflag: tar.TypeReg, mode: 0o644, body: "deep\n"},
		{name: "./a", typeflag: tar.TypeDir, mode: 0o700},
		// Hardlink BEFORE its target (flattened tars walk layers
		// top-down), plus one whose target never shows up.
		{name: "usr/bin/alias", typeflag: tar.TypeLink, link: "usr/bin/tool"},
		{name: "usr/bin/dangling", typeflag: tar.TypeLink, link: "usr/bin/gone"},
		{name: "usr/bin/tool", typeflag: tar.TypeReg, mode: 0o4755, uid: 1000, gid: 1000, body: "#!/bin/sh\n"},
		{name: "etc/hostname", typeflag: tar.TypeReg, mode: 0o644, body: "box\n"},
		{name: "bin/sh", typeflag: tar.TypeSymlink, mode: 0o777, link: "/usr/bin/tool"},
		{name: "dev/null", typeflag: tar.TypeChar, mode: 0o666, devmajor: 1, devminor: 3},
		{name: "etc/cap", typeflag: tar.TypeReg, mode: 0o644, body: "x", xattrs: map[string]string{"user.note": "hi"}},
	}), Writable(), TotalSize(32<<20))

	e2fsckClean(t, image)

	t.Run("setuid ownership preserved", func(t *testing.T) {
		out := debugfsStat(t, image, "/usr/bin/tool")
		for _, want := range []string{"Type: regular", "User:  1000", "Group:  1000", "Mode:  04755", "Links: 2"} {
			if !strings.Contains(out, want) {
				t.Errorf("stat /usr/bin/tool: missing %q in:\n%s", want, out)
			}
		}
	})
	t.Run("deferred hardlink resolves", func(t *testing.T) {
		out := debugfsStat(t, image, "/usr/bin/alias")
		if !strings.Contains(out, "Links: 2") {
			t.Errorf("alias not linked to tool:\n%s", out)
		}
	})
	t.Run("dangling hardlink dropped", func(t *testing.T) {
		out := debugfsCmd(t, image, "ls /usr/bin")
		if strings.Contains(out, "dangling") {
			t.Errorf("dangling hardlink materialized:\n%s", out)
		}
	})
	t.Run("late parent directory wins", func(t *testing.T) {
		out := debugfsStat(t, image, "/a")
		if !strings.Contains(out, "Mode:  0700") {
			t.Errorf("directory entry arriving after child did not update mode:\n%s", out)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		out := debugfsStat(t, image, "/bin/sh")
		if !strings.Contains(out, "Type: symlink") || !strings.Contains(out, "/usr/bin/tool") {
			t.Errorf("stat /bin/sh:\n%s", out)
		}
	})
	t.Run("device node", func(t *testing.T) {
		out := debugfsStat(t, image, "/dev/null")
		if !strings.Contains(out, "Type: character special") {
			t.Errorf("stat /dev/null:\n%s", out)
		}
	})
	t.Run("xattr preserved", func(t *testing.T) {
		out := debugfsCmd(t, image, "ea_list /etc/cap")
		if !strings.Contains(out, "user.note") {
			t.Errorf("ea_list /etc/cap: missing user.note:\n%s", out)
		}
	})
	t.Run("file content", func(t *testing.T) {
		out := debugfsCmd(t, image, "cat /etc/hostname")
		if !strings.Contains(out, "box") {
			t.Errorf("cat /etc/hostname:\n%s", out)
		}
	})
}

func TestConvertRejectsWhiteout(t *testing.T) {
	var sink bytes.Buffer
	tarBuf := buildTar(t, []tarEntry{
		{name: "etc/.wh.motd", typeflag: tar.TypeReg, mode: 0o644},
	})
	err := Convert(tarBuf, nopSeeker{&sink})
	require.Error(t, err, "Convert with whiteout entry should fail")
	require.Contains(t, err.Error(), "whiteout", "Convert with whiteout entry: err = %v, want whiteout rejection", err)
}

// nopSeeker adapts a buffer for Convert's signature in error-path tests
// that never reach a real seek.
type nopSeeker struct{ *bytes.Buffer }

func (nopSeeker) Seek(offset int64, whence int) (int64, error) { return 0, nil }
func (n nopSeeker) Read(p []byte) (int, error)                 { return n.Buffer.Read(p) }

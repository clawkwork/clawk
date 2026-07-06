package oci

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clawkwork/clawk/machine"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/stretchr/testify/require"
)

type layerEntry struct {
	name     string
	typeflag byte
	mode     int64
	uid, gid int
	body     string
}

func layerFromEntries(t *testing.T, entries []layerEntry) v1.Layer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		err := tw.WriteHeader(&tar.Header{
			Name:     e.name,
			Typeflag: e.typeflag,
			Mode:     e.mode,
			Uid:      e.uid,
			Gid:      e.gid,
			Size:     int64(len(e.body)),
		})
		require.NoError(t, err, "layer header %q", e.name)
		_, err = io.WriteString(tw, e.body)
		require.NoError(t, err, "layer body %q", e.name)
	}
	err := tw.Close()
	require.NoError(t, err)
	raw := buf.Bytes()
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(raw)), nil
	})
	require.NoError(t, err, "layer")
	return layer
}

// pushTestImage assembles a two-layer image exercising whiteouts, opaque
// directories and ownership, pushes it to an in-process registry, and
// returns its reference.
func pushTestImage(t *testing.T) string {
	t.Helper()
	base := layerFromEntries(t, []layerEntry{
		{name: "etc/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "etc/motd", typeflag: tar.TypeReg, mode: 0o644, body: "delete me\n"},
		{name: "etc/passwd", typeflag: tar.TypeReg, mode: 0o644, body: "root:v1\n"},
		{name: "opt/doc/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "opt/doc/old", typeflag: tar.TypeReg, mode: 0o644, body: "stale\n"},
	})
	top := layerFromEntries(t, []layerEntry{
		{name: "etc/.wh.motd", typeflag: tar.TypeReg, mode: 0o644},
		{name: "etc/passwd", typeflag: tar.TypeReg, mode: 0o644, body: "root:v2\n"},
		{name: "opt/doc/.wh..wh..opq", typeflag: tar.TypeReg, mode: 0o644},
		{name: "opt/doc/new", typeflag: tar.TypeReg, mode: 0o644, body: "fresh\n"},
		{name: "usr/bin/tool", typeflag: tar.TypeReg, mode: 0o4755, uid: 1000, gid: 1000, body: "#!/bin/sh\n"},
	})
	img, err := mutate.AppendLayers(empty.Image, base, top)
	require.NoError(t, err, "assembling image")

	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	ref := strings.TrimPrefix(srv.URL, "http://") + "/test/rootfs:latest"
	err = crane.Push(img, ref)
	require.NoError(t, err, "pushing to in-process registry")
	return ref
}

// TestBuild runs the full pipeline against an in-process registry — no
// network, no privileges. Filesystem content is verified with debugfs.
func TestBuild(t *testing.T) {
	requireTool(t, "debugfs")
	requireTool(t, "dumpe2fs")

	ref := pushTestImage(t)
	cacheDir := t.TempDir()
	res, err := Build(context.Background(), Options{
		Ref:        ref,
		CacheDir:   cacheDir,
		MinSizeMiB: 64,
	})
	require.NoError(t, err, "Build")
	require.NotZero(t, res.UnpackedBytes, "UnpackedBytes = 0 on a fresh build")
	disk := res.DiskPath

	t.Run("whiteout applied", func(t *testing.T) {
		if listing := debugfsRun(t, disk, "ls /etc"); strings.Contains(listing, "motd") {
			t.Errorf("whited-out /etc/motd still present:\n%s", listing)
		}
	})
	t.Run("upper layer wins", func(t *testing.T) {
		if got := debugfsRun(t, disk, "cat /etc/passwd"); !strings.Contains(got, "root:v2") {
			t.Errorf("cat /etc/passwd = %q, want layer-2 content", got)
		}
	})
	t.Run("opaque directory cleared", func(t *testing.T) {
		listing := debugfsRun(t, disk, "ls /opt/doc")
		if strings.Contains(listing, "old") {
			t.Errorf("opaque whiteout left /opt/doc/old behind:\n%s", listing)
		}
		if !strings.Contains(listing, "new") {
			t.Errorf("/opt/doc/new missing:\n%s", listing)
		}
	})
	t.Run("ownership and setuid preserved", func(t *testing.T) {
		out := debugfsRun(t, disk, "stat /usr/bin/tool")
		for _, want := range []string{"User:  1000", "Group:  1000", "Mode:  04755"} {
			if !strings.Contains(out, want) {
				t.Errorf("stat /usr/bin/tool: missing %q in:\n%s", want, out)
			}
		}
	})
	t.Run("writable with free space", func(t *testing.T) {
		out, err := exec.Command("dumpe2fs", "-h", disk).CombinedOutput()
		require.NoError(t, err, "dumpe2fs:\n%s", out)
		h := string(out)
		require.False(t, strings.Contains(h, "read-only"), "filesystem carries the read-only feature:\n%s", h)
		free := headerValue(t, h, "Free blocks:")
		blocks := headerValue(t, h, "Block count:")
		require.GreaterOrEqual(t, blocks, int64((64<<20)/4096), "block count want >= 64 MiB worth")
		require.GreaterOrEqual(t, free, blocks/2, "free blocks = %d of %d, want most of the padding free", free, blocks)
	})
	t.Run("no unpacked tree left on disk", func(t *testing.T) {
		_, err := os.Stat(filepath.Join(filepath.Dir(disk), "rootfs"))
		require.True(t, os.IsNotExist(err), "unexpected rootfs/ tree next to the disk (err=%v)", err)
	})
	t.Run("layer cache populated", func(t *testing.T) {
		entries, err := os.ReadDir(filepath.Join(cacheDir, "layers"))
		require.NoError(t, err)
		require.NotEmpty(t, entries, "layer cache empty")
	})
	t.Run("cache hit", func(t *testing.T) {
		res2, err := Build(context.Background(), Options{Ref: ref, CacheDir: cacheDir, MinSizeMiB: 64})
		require.NoError(t, err, "cached Build")
		require.Equal(t, disk, res2.DiskPath, "cache miss")
	})
}

// TestBuildInject verifies injected files land in the disk on top of the
// image content, that injection changes the cache key, and that the
// existing image content survives untouched.
func TestBuildInject(t *testing.T) {
	requireTool(t, "debugfs")

	ref := pushTestImage(t)
	cacheDir := t.TempDir()

	initBin := filepath.Join(t.TempDir(), "clawk-init")
	err := os.WriteFile(initBin, []byte("#!/bin/sh\necho fake init\n"), 0o644)
	require.NoError(t, err)
	inject := []machine.InjectFile{
		{GuestPath: "/sbin/clawk-init", HostPath: initBin, Mode: 0o755},
		// Deep destination: parents must be created on demand.
		{GuestPath: "/opt/clawk/bin/agent", HostPath: initBin, Mode: 0o755},
		// Overwrite of existing image content: inject wins.
		{GuestPath: "/etc/passwd", HostPath: initBin, Mode: 0o644},
	}

	res, err := Build(context.Background(), Options{
		Ref: ref, CacheDir: cacheDir, MinSizeMiB: 64, Inject: inject,
	})
	require.NoError(t, err, "Build with inject")

	t.Run("injected files present with mode", func(t *testing.T) {
		out := debugfsRun(t, res.DiskPath, "stat /sbin/clawk-init")
		for _, want := range []string{"Mode:  0755", "User:     0"} {
			if !strings.Contains(out, want) {
				t.Errorf("stat /sbin/clawk-init: missing %q in:\n%s", want, out)
			}
		}
		if got := debugfsRun(t, res.DiskPath, "cat /opt/clawk/bin/agent"); !strings.Contains(got, "fake init") {
			t.Errorf("cat /opt/clawk/bin/agent = %q", got)
		}
	})
	t.Run("inject overwrites image content", func(t *testing.T) {
		if got := debugfsRun(t, res.DiskPath, "cat /etc/passwd"); !strings.Contains(got, "fake init") {
			t.Errorf("inject did not win over image /etc/passwd: %q", got)
		}
	})
	t.Run("image content untouched elsewhere", func(t *testing.T) {
		if got := debugfsRun(t, res.DiskPath, "cat /opt/doc/new"); !strings.Contains(got, "fresh") {
			t.Errorf("cat /opt/doc/new = %q", got)
		}
	})
	t.Run("inject changes cache key", func(t *testing.T) {
		plain, err := Build(context.Background(), Options{Ref: ref, CacheDir: cacheDir, MinSizeMiB: 64})
		require.NoError(t, err, "Build without inject")
		require.NotEqual(t, res.DiskPath, plain.DiskPath, "inject-free build shares a disk with the injected build")
	})
	t.Run("content change changes cache key", func(t *testing.T) {
		err := os.WriteFile(initBin, []byte("#!/bin/sh\necho v2\n"), 0o644)
		require.NoError(t, err)
		res2, err := Build(context.Background(), Options{
			Ref: ref, CacheDir: cacheDir, MinSizeMiB: 64, Inject: inject,
		})
		require.NoError(t, err, "Build with changed inject")
		require.NotEqual(t, res.DiskPath, res2.DiskPath, "changed inject content reused the old disk")
	})
	t.Run("missing inject file fails fast", func(t *testing.T) {
		_, err := Build(context.Background(), Options{
			Ref: ref, CacheDir: cacheDir, MinSizeMiB: 64,
			Inject: []machine.InjectFile{{GuestPath: "/x", HostPath: filepath.Join(t.TempDir(), "nope"), Mode: 0o644}},
		})
		require.Error(t, err, "Build succeeded with unreadable inject file")
	})
}

// TestBuildInjectMergedUsr reproduces the Debian/Ubuntu merged-usr
// layout — /sbin, /bin, /lib are symlinks into /usr — and verifies
// injection through those symlinks lands in the real directory. This is
// how golang:*, ubuntu:* etc. are laid out; a converter that walks
// directory children literally fails with "path not found" here.
func TestBuildInjectMergedUsr(t *testing.T) {
	requireTool(t, "debugfs")

	layer := layerFromEntries(t, []layerEntry{
		{name: "usr/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "usr/sbin/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "usr/lib/", typeflag: tar.TypeDir, mode: 0o755},
	})
	links := linkLayer(t, map[string]string{
		"sbin": "usr/sbin", // relative target (Debian's actual shape)
		"lib":  "/usr/lib", // absolute target
		"bin":  "sbin",     // chained: symlink → symlink → dir
	})
	img, err := mutate.AppendLayers(empty.Image, layer, links)
	require.NoError(t, err)
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	ref := strings.TrimPrefix(srv.URL, "http://") + "/test/mergedusr:latest"
	err = crane.Push(img, ref)
	require.NoError(t, err)

	bin := filepath.Join(t.TempDir(), "payload")
	err = os.WriteFile(bin, []byte("payload\n"), 0o644)
	require.NoError(t, err)
	res, err := Build(context.Background(), Options{
		Ref: ref, CacheDir: t.TempDir(), MinSizeMiB: 64,
		Inject: []machine.InjectFile{
			{GuestPath: "/sbin/clawk-init", HostPath: bin, Mode: 0o755},
			{GuestPath: "/lib/marker", HostPath: bin, Mode: 0o644},
			{GuestPath: "/bin/tool", HostPath: bin, Mode: 0o755},
			// New subdir behind a symlinked ancestor.
			{GuestPath: "/sbin/sub/deep", HostPath: bin, Mode: 0o644},
		},
	})
	require.NoError(t, err, "Build with merged-usr inject")

	for _, p := range []string{
		"/usr/sbin/clawk-init", "/usr/lib/marker", "/usr/sbin/tool", "/usr/sbin/sub/deep",
	} {
		if got := debugfsRun(t, res.DiskPath, "cat "+p); !strings.Contains(got, "payload") {
			t.Errorf("cat %s = %q, want injected payload", p, got)
		}
	}
}

// TestBuildFromTarball builds from a `docker save`-style tarball path —
// the no-registry workflow for locally built images. Inject and cache
// behavior must match the registry path.
func TestBuildFromTarball(t *testing.T) {
	requireTool(t, "debugfs")

	layer := layerFromEntries(t, []layerEntry{
		{name: "etc/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "etc/banner", typeflag: tar.TypeReg, mode: 0o644, body: "from tarball\n"},
	})
	img, err := mutate.AppendLayers(empty.Image, layer)
	require.NoError(t, err)
	tarPath := filepath.Join(t.TempDir(), "local-image.tar")
	ref, err := name.NewTag("clawk-dev:v0")
	require.NoError(t, err)
	err = tarball.WriteToFile(tarPath, ref, img)
	require.NoError(t, err, "writing docker-save tarball")

	payload := filepath.Join(t.TempDir(), "payload")
	err = os.WriteFile(payload, []byte("init\n"), 0o644)
	require.NoError(t, err)

	cacheDir := t.TempDir()
	res, err := Build(context.Background(), Options{
		Ref: tarPath, CacheDir: cacheDir, MinSizeMiB: 64,
		Inject: []machine.InjectFile{{GuestPath: "/sbin/clawk-init", HostPath: payload, Mode: 0o755}},
	})
	require.NoError(t, err, "Build from tarball")
	if got := debugfsRun(t, res.DiskPath, "cat /etc/banner"); !strings.Contains(got, "from tarball") {
		t.Errorf("cat /etc/banner = %q", got)
	}
	if got := debugfsRun(t, res.DiskPath, "cat /sbin/clawk-init"); !strings.Contains(got, "init") {
		t.Errorf("inject missing: %q", got)
	}

	t.Run("cache hit without reopening", func(t *testing.T) {
		res2, err := Build(context.Background(), Options{
			Ref: tarPath, CacheDir: cacheDir, MinSizeMiB: 64,
			Inject: []machine.InjectFile{{GuestPath: "/sbin/clawk-init", HostPath: payload, Mode: 0o755}},
		})
		require.NoError(t, err, "cached Build")
		require.Equal(t, res.DiskPath, res2.DiskPath, "cache miss")
	})
	t.Run("missing tarball errors", func(t *testing.T) {
		_, err := Build(context.Background(), Options{
			Ref: filepath.Join(t.TempDir(), "nope.tar"), CacheDir: cacheDir, MinSizeMiB: 64,
		})
		require.Error(t, err, "Build succeeded on a missing tarball")
	})
}

// TestBuildProgress: a real build announces itself once with done == 0
// and the compressed-size scale; a cache hit never calls Progress.
func TestBuildProgress(t *testing.T) {
	ref := pushTestImage(t)
	cacheDir := t.TempDir()

	var calls []ProgressUpdate
	res, err := Build(context.Background(), Options{
		Ref: ref, CacheDir: cacheDir, MinSizeMiB: 64,
		Progress: func(u ProgressUpdate) { calls = append(calls, u) },
	})
	require.NoError(t, err, "Build")
	require.NotZero(t, res.UnpackedBytes, "expected a fresh build")
	require.NotEmpty(t, calls, "expected progress calls")
	require.Zero(t, calls[0].UnpackedBytes, "first progress UnpackedBytes should be 0, got %+v", calls[0])
	require.Positive(t, calls[0].CompressedTotal, "first progress CompressedTotal should be > 0, got %+v", calls[0])
	require.Equal(t, 2, calls[0].Layers, "initial update should carry the layer count (2), got %+v", calls[0])

	// A parallel download phase reports one entry per layer; its final
	// frame shows every layer fully fetched. (Unpack updates only fire
	// per 8 MiB converted, so this tiny image emits none.)
	var lastDownload *ProgressUpdate
	for i := range calls {
		if calls[i].Phase == PhaseDownload {
			require.Len(t, calls[i].Downloads, 2, "download update should report 2 layers")
			lastDownload = &calls[i]
		}
	}
	require.NotNil(t, lastDownload, "no PhaseDownload update; layers should download in parallel before unpack")
	for _, d := range lastDownload.Downloads {
		require.True(t, d.Done, "layer %d not done in final frame: %+v", d.Index, d)
		require.NotZero(t, d.Downloaded, "layer %d Downloaded=0 in final frame: %+v", d.Index, d)
	}

	calls = nil
	_, err = Build(context.Background(), Options{
		Ref: ref, CacheDir: cacheDir, MinSizeMiB: 64,
		Progress: func(u ProgressUpdate) { calls = append(calls, u) },
	})
	require.NoError(t, err, "cached Build")
	require.Empty(t, calls, "cache hit invoked Progress: %+v", calls)
}

// TestBuildImageEnv verifies the image config's Env is baked into the
// filesystem — the raw KEY=VALUE list and the profile.d export script —
// since a VM boot has no container runtime to inject it.
func TestBuildImageEnv(t *testing.T) {
	requireTool(t, "debugfs")

	layer := layerFromEntries(t, []layerEntry{
		{name: "etc/", typeflag: tar.TypeDir, mode: 0o755},
	})
	img, err := mutate.AppendLayers(empty.Image, layer)
	require.NoError(t, err)
	cf, err := img.ConfigFile()
	require.NoError(t, err)
	cf = cf.DeepCopy()
	cf.Config.Env = []string{
		"PATH=/usr/local/go/bin:/usr/bin:/bin",
		"GOLANG_VERSION=1.25.0",
		"QUOTED=it's quoted",
	}
	img, err = mutate.ConfigFile(img, cf)
	require.NoError(t, err)

	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	ref := strings.TrimPrefix(srv.URL, "http://") + "/test/envimg:latest"
	err = crane.Push(img, ref)
	require.NoError(t, err)

	res, err := Build(context.Background(), Options{Ref: ref, CacheDir: t.TempDir(), MinSizeMiB: 64})
	require.NoError(t, err, "Build")

	raw := debugfsRun(t, res.DiskPath, "cat "+ImageEnvPath)
	for _, want := range []string{"PATH=/usr/local/go/bin:/usr/bin:/bin", "GOLANG_VERSION=1.25.0"} {
		if !strings.Contains(raw, want) {
			t.Errorf("%s missing %q:\n%s", ImageEnvPath, want, raw)
		}
	}
	script := debugfsRun(t, res.DiskPath, "cat /etc/profile.d/00-clawk-image-env.sh")
	for _, want := range []string{
		"export PATH='/usr/local/go/bin:/usr/bin:/bin'",
		`export QUOTED='it'\''s quoted'`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("profile.d script missing %q:\n%s", want, script)
		}
	}
}

// linkLayer builds a layer of symlinks (name → target).
func linkLayer(t *testing.T, links map[string]string) v1.Layer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, target := range links {
		err := tw.WriteHeader(&tar.Header{
			Name: name, Typeflag: tar.TypeSymlink, Linkname: target, Mode: 0o777,
		})
		require.NoError(t, err)
	}
	err := tw.Close()
	require.NoError(t, err)
	raw := buf.Bytes()
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(raw)), nil
	})
	require.NoError(t, err)
	return layer
}

// headerValue extracts the integer following a dumpe2fs -h header label.
func headerValue(t *testing.T, header, label string) int64 {
	t.Helper()
	i := strings.Index(header, label)
	require.GreaterOrEqual(t, i, 0, "dumpe2fs header missing %q:\n%s", label, header)
	fields := strings.Fields(header[i+len(label):])
	require.NotEmpty(t, fields, "dumpe2fs header has no value for %q", label)
	var n int64
	for _, c := range fields[0] {
		require.True(t, c >= '0' && c <= '9', "non-numeric %q value %q", label, fields[0])
		n = n*10 + int64(c-'0')
	}
	return n
}

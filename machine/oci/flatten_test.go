package oci

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/stretchr/testify/require"
)

// flattenToMap runs flatten over an image assembled from the given layers
// and returns path → content for every emitted entry.
func flattenToMap(t *testing.T, layers ...v1.Layer) map[string]string {
	t.Helper()
	img, err := mutate.AppendLayers(empty.Image, layers...)
	require.NoError(t, err, "assembling image")
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	err = flatten(img, tw, nil, nil)
	require.NoError(t, err, "flatten")
	err = tw.Close()
	require.NoError(t, err)

	got := map[string]string{}
	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return got
		}
		require.NoError(t, err, "reading flattened tar")
		_, dup := got[hdr.Name]
		require.False(t, dup, "duplicate entry %q in flattened tar", hdr.Name)
		body, err := io.ReadAll(tr)
		require.NoError(t, err, "reading %q", hdr.Name)
		got[hdr.Name] = string(body)
	}
}

func TestFlatten(t *testing.T) {
	tests := []struct {
		name   string
		layers [][]layerEntry
		want   map[string]string // path → content; "" for dirs
		absent []string
	}{
		{
			name: "explicit whiteout deletes lower file",
			layers: [][]layerEntry{
				{
					{name: "etc/", typeflag: tar.TypeDir, mode: 0o755},
					{name: "etc/motd", typeflag: tar.TypeReg, mode: 0o644, body: "bye"},
				},
				{
					{name: "etc/.wh.motd", typeflag: tar.TypeReg},
				},
			},
			want:   map[string]string{"etc": ""},
			absent: []string{"etc/motd", "etc/.wh.motd"},
		},
		{
			name: "opaque whiteout hides lower content but keeps same-layer siblings",
			layers: [][]layerEntry{
				{
					{name: "opt/doc/", typeflag: tar.TypeDir, mode: 0o755},
					{name: "opt/doc/old", typeflag: tar.TypeReg, mode: 0o644, body: "stale"},
					{name: "opt/doc/sub/", typeflag: tar.TypeDir, mode: 0o755},
					{name: "opt/doc/sub/deep", typeflag: tar.TypeReg, mode: 0o644, body: "deep"},
				},
				{
					{name: "opt/doc/.wh..wh..opq", typeflag: tar.TypeReg},
					{name: "opt/doc/new", typeflag: tar.TypeReg, mode: 0o644, body: "fresh"},
				},
			},
			want:   map[string]string{"opt/doc/new": "fresh"},
			absent: []string{"opt/doc/old", "opt/doc/sub", "opt/doc/sub/deep"},
		},
		{
			name: "upper file wins and path normalization dedups",
			layers: [][]layerEntry{
				{
					{name: "./etc/passwd", typeflag: tar.TypeReg, mode: 0o644, body: "v1"},
				},
				{
					{name: "/etc/passwd", typeflag: tar.TypeReg, mode: 0o644, body: "v2"},
				},
			},
			want: map[string]string{"etc/passwd": "v2"},
		},
		{
			name: "directories merge across layers",
			layers: [][]layerEntry{
				{
					{name: "usr/", typeflag: tar.TypeDir, mode: 0o755},
					{name: "usr/lower", typeflag: tar.TypeReg, mode: 0o644, body: "a"},
				},
				{
					{name: "usr/", typeflag: tar.TypeDir, mode: 0o755},
					{name: "usr/upper", typeflag: tar.TypeReg, mode: 0o644, body: "b"},
				},
			},
			want: map[string]string{"usr": "", "usr/lower": "a", "usr/upper": "b"},
		},
		{
			name: "file replacing directory hides its children",
			layers: [][]layerEntry{
				{
					{name: "data/", typeflag: tar.TypeDir, mode: 0o755},
					{name: "data/child", typeflag: tar.TypeReg, mode: 0o644, body: "x"},
				},
				{
					{name: "data", typeflag: tar.TypeReg, mode: 0o644, body: "now a file"},
				},
			},
			want:   map[string]string{"data": "now a file"},
			absent: []string{"data/child"},
		},
		{
			name: "whiteout then recreate in upper layer",
			layers: [][]layerEntry{
				{
					{name: "a/", typeflag: tar.TypeDir, mode: 0o755},
					{name: "a/f", typeflag: tar.TypeReg, mode: 0o644, body: "old"},
				},
				{
					{name: ".wh.a", typeflag: tar.TypeReg},
				},
				{
					{name: "a/", typeflag: tar.TypeDir, mode: 0o755},
					{name: "a/g", typeflag: tar.TypeReg, mode: 0o644, body: "new"},
				},
			},
			want:   map[string]string{"a": "", "a/g": "new"},
			absent: []string{"a/f"},
		},
		{
			name: "aufs metadata skipped",
			layers: [][]layerEntry{
				{
					{name: ".wh..wh.plnk/", typeflag: tar.TypeDir, mode: 0o755},
					{name: ".wh..wh.aufs", typeflag: tar.TypeReg},
					{name: "keep", typeflag: tar.TypeReg, mode: 0o644, body: "k"},
				},
			},
			want:   map[string]string{"keep": "k"},
			absent: []string{".wh..wh.plnk", ".wh..wh.aufs"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			layers := make([]v1.Layer, len(tt.layers))
			for i, entries := range tt.layers {
				layers[i] = layerFromEntries(t, entries)
			}
			got := flattenToMap(t, layers...)
			for p, content := range tt.want {
				body, ok := got[p]
				require.True(t, ok, "missing %q in flattened output: %v", p, keys(got))
				if content != "" {
					require.Equal(t, content, body, "%q content mismatch", p)
				}
			}
			for _, p := range tt.absent {
				_, ok := got[p]
				require.False(t, ok, "%q present in flattened output, want absent", p)
			}
		})
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

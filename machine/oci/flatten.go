package oci

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// OCI whiteout markers (image-spec layer.md). A `.wh.<name>` entry deletes
// <name> from lower layers; a `.wh..wh..opq` entry hides everything the
// lower layers put in its directory. Other `.wh..wh.` entries are AUFS
// bookkeeping with no filesystem meaning.
const (
	whiteoutPrefix = ".wh."
	whiteoutMeta   = ".wh..wh."
	opaqueWhiteout = ".wh..wh..opq"
)

// flatten writes img's merged filesystem to w as a single tar stream,
// applying OCI whiteout semantics. Layers are walked top-down so each path
// is emitted at most once, from the layer that wins.
//
// go-containerregistry's mutate.Extract is almost this, but it ignores
// opaque whiteouts (`.wh..wh..opq`), so images built by tools that emit
// them (buildah, podman, anything overlayfs-backed) keep files their
// upper layers deleted. Hence our own implementation.
//
// shadow lists paths the caller will write itself after the flatten (see
// appendInject) — they act like an extra topmost layer: the image's
// entries at those exact paths are suppressed so the ext4 converter never
// sees a file written twice (it cannot overwrite committed file data).
//
// onLayer, if non-nil, is called as each layer's processing begins with
// the 1-based position and the layer count — progress display.
func flatten(img v1.Image, w *tar.Writer, shadow []string, onLayer func(pos, count int)) error {
	layers, err := img.Layers()
	if err != nil {
		return fmt.Errorf("reading layers: %w", err)
	}

	// seen tracks every path already decided. true means the path also
	// shadows its children (deleted, or replaced by a non-directory);
	// false means only the entry itself is taken (directories merge).
	seen := map[string]bool{}
	for _, s := range shadow {
		// false: only the entry itself is replaced; an injected file never
		// hides a directory's children (inject paths are regular files).
		seen[path.Clean(strings.TrimPrefix(s, "/"))] = false
	}
	// opaque holds directories whose lower-layer content is hidden.
	// Markers found in layer i take effect from layer i-1 down, so each
	// layer's markers are staged in pending until the layer is done —
	// sibling files in the marker's own layer are kept.
	opaque := map[string]bool{}

	for i := len(layers) - 1; i >= 0; i-- {
		if onLayer != nil {
			onLayer(len(layers)-i, len(layers))
		}
		pending := map[string]bool{}
		if err := flattenLayer(layers[i], w, seen, opaque, pending); err != nil {
			return fmt.Errorf("layer %d: %w", i, err)
		}
		for dir := range pending {
			opaque[dir] = true
		}
	}
	return nil
}

// flattenLayer streams one layer through the whiteout filter into w.
func flattenLayer(layer v1.Layer, w *tar.Writer, seen, opaque, pending map[string]bool) error {
	rc, err := layer.Uncompressed()
	if err != nil {
		return fmt.Errorf("opening layer: %w", err)
	}
	defer rc.Close()

	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading layer tar: %w", err)
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}

		// Normalize: tools variously emit "./etc/passwd", "/etc/passwd"
		// and "etc/passwd" for the same path; the dedup map needs one form.
		name := path.Clean(strings.TrimPrefix(hdr.Name, "/"))
		if name == "." || name == ".." || strings.HasPrefix(name, "../") {
			continue
		}

		dir, base := path.Dir(name), path.Base(name)

		switch {
		case base == opaqueWhiteout:
			pending[dir] = true
			continue
		case strings.HasPrefix(base, whiteoutMeta):
			// AUFS plink/metadata entries — not filesystem content.
			continue
		case strings.HasPrefix(base, whiteoutPrefix):
			// Explicit deletion: tombstone the target for lower layers.
			// Unconditional — even if an upper layer re-created the
			// target as a directory, this layer's whiteout still hides
			// the lower layers' children.
			seen[path.Join(dir, strings.TrimPrefix(base, whiteoutPrefix))] = true
			continue
		}

		if _, ok := seen[name]; ok {
			// Already emitted from an upper layer, deleted, or replaced.
			// Directories merge: the entry is skipped but children from
			// this layer keep flowing unless an ancestor shadows them.
			continue
		}
		if shadowed(seen, opaque, name) {
			continue
		}

		hdr.Name = name
		// PAX avoids USTAR's 100-char name truncation on deep paths.
		hdr.Format = tar.FormatPAX
		if err := w.WriteHeader(hdr); err != nil {
			return fmt.Errorf("writing header %q: %w", name, err)
		}
		if hdr.Size > 0 {
			if _, err := io.CopyN(w, tr, hdr.Size); err != nil {
				return fmt.Errorf("writing %q: %w", name, err)
			}
		}

		// A non-directory at this path shadows anything below it in
		// lower layers (a file replacing a directory hides the
		// directory's children too).
		seen[name] = hdr.Typeflag != tar.TypeDir
	}
}

// shadowed reports whether any ancestor of name — including "." for a
// root-level opaque marker — is tombstoned, replaced by a non-directory,
// or opaque.
func shadowed(seen, opaque map[string]bool, name string) bool {
	for dir := path.Dir(name); ; dir = path.Dir(dir) {
		if seen[dir] || opaque[dir] {
			return true
		}
		if dir == "." || dir == "/" {
			return false
		}
	}
}

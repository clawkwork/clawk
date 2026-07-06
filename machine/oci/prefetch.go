package oci

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/cache"
	"golang.org/x/sync/errgroup"
)

// maxParallelDownloads bounds concurrent layer blob fetches, mirroring
// docker pull's default. Flattening stays strictly sequential regardless
// (OCI whiteout ordering demands it); this only parallelizes the
// network-bound download that used to be serialized inside the flatten.
const maxParallelDownloads = 3

// reportInterval paces per-layer download snapshots — frequent enough for
// smooth progress bars, rare enough to stay off the transfer hot path.
const reportInterval = 120 * time.Millisecond

// layerFetcher opens a fresh registry handle for one layer blob, routing
// its transfer through counter so each layer's bytes are metered
// independently. Each layer needs its own transport: a shared one cannot
// attribute bytes once a registry 307-redirects a blob to a CDN whose URL
// no longer carries the digest. nil for local (tarball) sources, which
// have nothing to download.
type layerFetcher func(ctx context.Context, digest v1.Hash, counter *atomic.Int64) (v1.Layer, error)

// knownLayer overrides a freshly fetched layer's identity with values
// already known from the image manifest and config. Without this, caching
// the layer would call Digest/DiffID/Size on the remote handle and pull
// the blob a second time just to compute them.
type knownLayer struct {
	v1.Layer
	digest, diffID v1.Hash
	size           int64
}

func (l knownLayer) Digest() (v1.Hash, error) { return l.digest, nil }
func (l knownLayer) DiffID() (v1.Hash, error) { return l.diffID, nil }
func (l knownLayer) Size() (int64, error)     { return l.size, nil }

// layerDownload tracks one layer's progress across the download phase.
type layerDownload struct {
	index  int     // 1-based position in the image manifest
	digest v1.Hash // compressed blob digest (cache key for Compressed)
	diffID v1.Hash // uncompressed digest (cache key for Uncompressed)
	size   int64   // compressed blob size, from the manifest
	got    atomic.Int64
	cached bool // already present locally; no download needed
	done   atomic.Bool
}

// prefetchLayers downloads every layer blob concurrently into c, so the
// sequential flatten that follows reads each layer warm from the cache
// instead of blocking on the network one layer at a time. This is the
// parallel-download, sequential-extract split that docker pull performs.
//
// report is invoked on a fixed interval (and once when the phase ends)
// with a per-layer snapshot, so the caller can drive one progress bar per
// layer. It is never called concurrently.
func prefetchLayers(ctx context.Context, c cache.Cache, layers []v1.Layer, fetch layerFetcher, report func([]LayerStatus)) error {
	dls := make([]*layerDownload, len(layers))
	for i, l := range layers {
		digest, err := l.Digest()
		if err != nil {
			return fmt.Errorf("layer %d digest: %w", i+1, err)
		}
		diffID, err := l.DiffID()
		if err != nil {
			return fmt.Errorf("layer %d diff id: %w", i+1, err)
		}
		size, err := l.Size()
		if err != nil {
			return fmt.Errorf("layer %d size: %w", i+1, err)
		}
		dl := &layerDownload{index: i + 1, digest: digest, diffID: diffID, size: size}
		// A layer already in the cache — a shared base layer, or a prior
		// build whose disk was wiped — needs no download. Show its bar
		// full from the first frame.
		if _, err := c.Get(diffID); err == nil {
			dl.cached = true
			dl.got.Store(size)
			dl.done.Store(true)
		}
		dls[i] = dl
	}

	stop := make(chan struct{})
	reported := make(chan struct{})
	go func() {
		defer close(reported)
		t := time.NewTicker(reportInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				report(snapshot(dls))
			}
		}
	}()

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(maxParallelDownloads)
	for _, dl := range dls {
		if dl.cached {
			continue
		}
		g.Go(func() error { return downloadLayer(ctx, c, fetch, dl) })
	}
	err := g.Wait()

	close(stop)
	<-reported
	report(snapshot(dls)) // final frame: surviving layers at 100%
	return err
}

// downloadLayer pulls one layer's blob through its own metered transport
// and tees the uncompressed result into the cache under its diffID, which
// is exactly the key the flatten loop later reads back.
func downloadLayer(ctx context.Context, c cache.Cache, fetch layerFetcher, dl *layerDownload) error {
	rl, err := fetch(ctx, dl.digest, &dl.got)
	if err != nil {
		return fmt.Errorf("layer %d (%s): %w", dl.index, dl.digest, err)
	}
	cl, err := c.Put(knownLayer{Layer: rl, digest: dl.digest, diffID: dl.diffID, size: dl.size})
	if err != nil {
		return fmt.Errorf("layer %d cache put: %w", dl.index, err)
	}
	rc, err := cl.Uncompressed()
	if err != nil {
		return fmt.Errorf("layer %d open: %w", dl.index, err)
	}
	if _, err := io.Copy(io.Discard, rc); err != nil {
		rc.Close()
		return fmt.Errorf("layer %d download: %w", dl.index, err)
	}
	// Close drives the cache's hash verification and atomic publish, so a
	// corrupt or short download fails here rather than silently re-pulling
	// (or worse, surfacing as an "unexpected EOF" deep in the flatten).
	if err := rc.Close(); err != nil {
		return fmt.Errorf("layer %d verify: %w", dl.index, err)
	}
	dl.done.Store(true)
	return nil
}

// snapshot reads the current per-layer counters into a report value.
func snapshot(dls []*layerDownload) []LayerStatus {
	out := make([]LayerStatus, len(dls))
	for i, dl := range dls {
		out[i] = LayerStatus{
			Index:          dl.index,
			CompressedSize: dl.size,
			Downloaded:     dl.got.Load(),
			Cached:         dl.cached,
			Done:           dl.done.Load(),
		}
	}
	return out
}

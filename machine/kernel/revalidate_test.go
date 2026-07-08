package kernel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
)

// TestOverrideRevalidates is the regression test for the stale-kernel bug: a
// kernel URL republished with new bytes at the same URL must be re-fetched, not
// served from a URL-keyed cache. It also checks the cheap path (unchanged →
// 304, no re-download) and the offline path (server down → keep the cache).
func TestOverrideRevalidates(t *testing.T) {
	var version int64 = 1
	var served int64 // count of 200 responses (actual downloads)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := atomic.LoadInt64(&version)
		etag := `"v` + itoa(v) + `"`
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		atomic.AddInt64(&served, 1)
		w.Header().Set("ETag", etag)
		_, _ = w.Write([]byte("kernel-v" + itoa(v)))
	}))
	defer srv.Close()

	dir := t.TempDir()
	opts := Options{CacheDir: dir, Override: srv.URL}
	read := func() string {
		p, err := Fetch(context.Background(), opts)
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		return string(b)
	}

	// 1. cold fetch downloads v1.
	if got := read(); got != "kernel-v1" {
		t.Fatalf("first fetch = %q, want kernel-v1", got)
	}
	// 2. unchanged → 304, no second download.
	if got := read(); got != "kernel-v1" {
		t.Fatalf("revalidated fetch = %q, want kernel-v1", got)
	}
	if n := atomic.LoadInt64(&served); n != 1 {
		t.Fatalf("downloads after revalidation = %d, want 1 (should have 304'd)", n)
	}
	// 3. republish new bytes at the same URL → must re-fetch (the bug).
	atomic.StoreInt64(&version, 2)
	if got := read(); got != "kernel-v2" {
		t.Fatalf("after republish = %q, want kernel-v2 (served stale cache?)", got)
	}

	// 4. offline: server gone → keep the last good cache rather than fail.
	srv.Close()
	if got := read(); got != "kernel-v2" {
		t.Fatalf("offline fetch = %q, want cached kernel-v2", got)
	}
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}

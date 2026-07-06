// Package debug is a single-knob diagnostic logger.
//
// Set CLAWK_DEBUG=1 in the environment to enable verbose output; every
// call to Log / Logf / Attr emits a timestamped line on stderr. Output is
// deliberately unstructured so a tail-f of vzd.log stays legible — the
// primary consumer is a human investigating a freeze, not a log ingester.
//
// The package is cheap when disabled: a single boolean read per call, no
// allocations. Safe to sprinkle across hot paths.
package debug

import (
	"fmt"
	"os"
	"sync"
	"time"
)

var (
	enabled = os.Getenv("CLAWK_DEBUG") != "" && os.Getenv("CLAWK_DEBUG") != "0"
	mu      sync.Mutex
)

// Enabled reports whether debug logging is active for this process.
// Callers can use this to skip expensive string construction when the
// output would be discarded anyway.
func Enabled() bool { return enabled }

// Log prints a single debug line to stderr when enabled. A UTC
// timestamp + the "[clawk-debug]" tag is prepended automatically.
//
// Format is intentionally "printf-ish" — we don't want a structured
// logger here because the signal-to-noise ratio for a human tailing
// the file is much better with free-form text. Pass tag plus message
// plus key=value pairs as you'd write them on paper.
func Log(tag, msg string, kv ...any) {
	if !enabled {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	ts := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	fmt.Fprintf(os.Stderr, "%s [clawk-debug] %s: %s", ts, tag, msg)
	// kv pairs printed as " key=val" — works for any fmt.Stringer or
	// primitive. Odd-count values pad with "?" rather than panicking,
	// because diagnostic logging should never crash the caller.
	for i := 0; i < len(kv); i += 2 {
		k := kv[i]
		var v any = "?"
		if i+1 < len(kv) {
			v = kv[i+1]
		}
		fmt.Fprintf(os.Stderr, " %v=%v", k, v)
	}
	fmt.Fprintln(os.Stderr)
}

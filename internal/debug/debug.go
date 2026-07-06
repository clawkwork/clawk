// Package debug is a single-knob diagnostic gate.
//
// Set CLAWK_DEBUG=1 in the environment to enable verbose behaviour across
// the CLI (extra daemon log lines, the SIGUSR1 goroutine dump, …). The check
// is cheap — a single boolean read, no allocations — so callers can gate
// expensive diagnostics on Enabled() even in hot paths.
package debug

import "os"

var enabled = os.Getenv("CLAWK_DEBUG") != "" && os.Getenv("CLAWK_DEBUG") != "0"

// Enabled reports whether debug diagnostics are active for this process.
func Enabled() bool { return enabled }

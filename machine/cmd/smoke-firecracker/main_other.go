//go:build !linux

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr,
		"smoke-firecracker is linux-only (firecracker requires /dev/kvm). "+
			"Run this inside a `clawk here --nested` guest on macOS.")
	os.Exit(1)
}

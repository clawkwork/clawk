package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/clawkwork/clawk/internal/cli"
	"github.com/clawkwork/clawk/internal/sandbox"
)

func main() {
	// Privileged mount/umount re-exec path — runs before cobra because these
	// hidden subcommands exist only for `sudo $self __loop-mount/-unmount`.
	sandbox.InitRootHelpers()
	err := cli.Execute()
	if err == nil {
		return
	}
	// An interactive guest session that exited non-zero is not a clawk
	// failure: relay its exact status as our own exit code, no message.
	var ee *sandbox.ExitError
	if errors.As(err, &ee) {
		os.Exit(ee.Code)
	}
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

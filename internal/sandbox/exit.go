package sandbox

import "fmt"

// ExitError reports that an interactive guest session (a shell or an
// attached agent) finished with a non-zero exit status that should become
// clawk's own process exit code.
//
// Provider Shell methods and the CLI's interactive-session helpers return
// it instead of calling os.Exit in place: that lets the cobra command's
// deferred cleanup run and keeps a single os.Exit site in cmd/clawk, which
// unwraps it with errors.As. Carrying the status as an error is also what
// preserves the exact code — a plain returned error collapses to exit 1.
//
// It is deliberately NOT used for internal command execution (ExecCapture,
// phase-setup scripts): those failures are ordinary errors that should
// surface a message and exit 1, not hijack the process exit code.
type ExitError struct{ Code int }

func (e *ExitError) Error() string {
	return fmt.Sprintf("guest session exited with status %d", e.Code)
}

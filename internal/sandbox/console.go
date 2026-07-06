package sandbox

import (
	"fmt"
	"os"
	"strings"
)

// ConsoleTail returns the last n non-empty lines of a guest console log,
// framed for embedding in a boot-failure message. Returns "" when the
// log is missing or empty — callers append it to their error text
// unconditionally.
//
// Boot failures are diagnosed from the guest console essentially every
// time (kernel panics, clawk-init errors, missing init binaries); making
// the CLI surface it directly turns "go read this file" into an answer.
func ConsoleTail(path string, n int) string {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return ""
	}
	var lines []string
	for _, l := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return fmt.Sprintf("\n--- last guest console output (%s) ---\n%s\n---",
		path, strings.Join(lines, "\n"))
}

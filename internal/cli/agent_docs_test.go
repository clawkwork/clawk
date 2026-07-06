package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/clawkwork/clawk/internal/template"
	"github.com/stretchr/testify/require"
)

func TestResolveAgentDocs(t *testing.T) {
	dir := t.TempDir()
	// Markdown content with the exact characters that defeat a DSL string
	// literal: double quotes, backticks, and a fenced code block.
	md := "# Conventions\n- run `make test`\n- prefer \"small\" funcs\n```go\nx := 1\n```\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "CONVENTIONS.md"), []byte(md), 0o644))

	got, err := resolveAgentDocs(dir, []template.AgentDoc{
		{Text: "inline one-liner"},
		{Path: "./CONVENTIONS.md"},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"inline one-liner", md}, got)
}

func TestResolveAgentDocsMissingFileErrors(t *testing.T) {
	_, err := resolveAgentDocs(t.TempDir(), []template.AgentDoc{{Path: "nope.md"}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "nope.md")
}

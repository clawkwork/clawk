package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/clawkwork/clawk/internal/template"
)

// resolveAgentDocs turns a repo's `agent (...)` entries into content strings:
// inline text passes through; a path is read from disk (relative to baseDir,
// the clawk.mod's directory) so its markdown — backticks, fences and all —
// reaches the agent verbatim without ever transiting the DSL's string lexer.
// Order is preserved so instructions read in the sequence they were declared.
func resolveAgentDocs(baseDir string, docs []template.AgentDoc) ([]string, error) {
	out := make([]string, 0, len(docs))
	for _, d := range docs {
		if d.Path == "" {
			out = append(out, d.Text)
			continue
		}
		p := d.Path
		if !filepath.IsAbs(p) {
			p = filepath.Join(baseDir, p)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("reading agent doc %q: %w", d.Path, err)
		}
		out = append(out, string(data))
	}
	return out, nil
}

package template

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ParseString parses a typed-block clawk file and returns its sandbox
// template. It is a thin wrapper over ParseFileString for callers that only
// care about the sandbox block; a file without one is an error. Legacy flat
// files fail inside ParseFileString with a wrap-in-sandbox migration hint.
func ParseString(src string) (*Template, error) {
	f, err := ParseFileString(src)
	if err != nil {
		return nil, err
	}
	if f.Sandbox == nil {
		return nil, errors.New("no sandbox block (want `sandbox [<name>] ( ... )`)")
	}
	return f.Sandbox, nil
}

// ExpandPath resolves leading ~ (home dir) and $HOME. It does NOT make the
// path absolute — callers decide the base directory to resolve against. In
// workspace mode that's the workspace root.
//
// Other env vars are NOT expanded: we don't want to silently substitute
// host-side secrets into template-visible paths.
func ExpandPath(p string) (string, error) {
	if strings.HasPrefix(p, "~/") || p == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if p == "~" {
			return home, nil
		}
		return filepath.Join(home, p[2:]), nil
	}
	if strings.HasPrefix(p, "$HOME/") || p == "$HOME" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if p == "$HOME" {
			return home, nil
		}
		return filepath.Join(home, p[len("$HOME/"):]), nil
	}
	return p, nil
}

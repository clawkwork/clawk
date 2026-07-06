package cli

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestShortImageRef(t *testing.T) {
	tests := []struct {
		ref, want string
	}{
		{"", "(none)"},
		{"docker.io/docker/sandbox-templates:claude-code", "docker/sandbox-templates:claude-code"},
		{"docker.io/library/alpine:3.20", "alpine:3.20"},
		{"ghcr.io/clawkwork/clawk-dev:v0", "ghcr.io/clawkwork/clawk-dev:v0"},
		{"/Users/u/clawk-dev.tar", "clawk-dev.tar"},
		{"./img.tar", "img.tar"},
	}
	for _, tt := range tests {
		require.Equal(t, tt.want, shortImageRef(tt.ref), "shortImageRef(%q)", tt.ref)
	}
}

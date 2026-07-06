package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFinalizeImageRef(t *testing.T) {
	setFlag := func(t *testing.T, v string) {
		t.Helper()
		old := imageFlag
		imageFlag = v
		t.Cleanup(func() { imageFlag = old })
	}

	tests := []struct {
		name string
		ref  string
		flag string
		want string
	}{
		{name: "default chain", ref: "", flag: "", want: defaultImage},
		{name: "flag fills missing clawk.mod", ref: "", flag: "ghcr.io/x/base:1", want: "ghcr.io/x/base:1"},
		{name: "flag overrides clawk.mod", ref: "golang:1.25", flag: "ghcr.io/x/base:1", want: "ghcr.io/x/base:1"},
		{name: "clawk.mod without flag", ref: "golang:1.25", flag: "", want: "golang:1.25"},
		{name: "explicit ref passthrough", ref: "alpine:3.20", flag: "", want: "alpine:3.20"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setFlag(t, tt.flag)
			require.Equal(t, tt.want, finalizeImageRef(tt.ref))
		})
	}

	t.Run("tilde expansion", func(t *testing.T) {
		setFlag(t, "")
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skip("no home dir")
		}
		require.Equal(t, filepath.Join(home, "img.tar"), finalizeImageRef("~/img.tar"))
	})
}

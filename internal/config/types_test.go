package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSandboxDisplayName(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want string
	}{
		{"named sandbox is unchanged", "my-feature", "my-feature"},
		{"anchored basename key is unchanged", "myproj", "myproj"},
		{"collision-disambiguated key is unchanged", "shared_a1b2c3", "shared_a1b2c3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sb := &Sandbox{Name: tt.key}
			require.Equal(t, tt.want, sb.DisplayName())
		})
	}
}

func TestSandboxNamespaceName(t *testing.T) {
	tests := []struct {
		name string
		ns   string
		want string
	}{
		{"empty resolves to default", "", DefaultNamespace},
		{"explicit value passes through", "team-a", "team-a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sb := &Sandbox{Namespace: tt.ns}
			require.Equal(t, tt.want, sb.NamespaceName())
		})
	}
}

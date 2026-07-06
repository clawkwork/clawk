package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNamespaceSaveLoad(t *testing.T) {
	s := testStore(t)
	want := &Namespace{
		Name:           "work",
		AllowedDomains: []string{"corp.example.com"},
		Files:          []HostFile{{HostPath: "/h/ctx.md", GuestPath: "/g/ctx.md"}},
		Env:            []string{"CORP_TOKEN"},
	}
	require.NoError(t, s.SaveNamespace(want))
	got, err := s.LoadNamespace("work")
	require.NoError(t, err)
	require.Len(t, got.AllowedDomains, 1)
	require.Equal(t, "corp.example.com", got.AllowedDomains[0])
	require.Len(t, got.Files, 1)
	require.Equal(t, "/g/ctx.md", got.Files[0].GuestPath)
}

func TestLoadNamespace_MissingIsEmptyNotError(t *testing.T) {
	s := testStore(t)
	got, err := s.LoadNamespace("nope")
	require.NoError(t, err, "LoadNamespace of missing should not error")
	require.Equal(t, "nope", got.Name)
	require.Empty(t, got.AllowedDomains)
	require.Empty(t, got.Files)
}

func TestListNamespaces(t *testing.T) {
	s := testStore(t)
	require.NoError(t, s.SaveNamespace(&Namespace{Name: "work"}))
	require.NoError(t, s.SaveNamespace(&Namespace{Name: "personal"}))
	list, err := s.ListNamespaces()
	require.NoError(t, err)
	require.Len(t, list, 2)
}

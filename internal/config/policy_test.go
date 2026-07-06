package config

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPolicySaveLoad(t *testing.T) {
	s := testStore(t)
	p := &Policy{
		Name:         "corp-egress",
		AllowDomains: []string{"github.com", "*.githubusercontent.com"},
		AllowIPs:     []string{"10.20.0.0/16"},
		DenyDomains:  []string{"telemetry.corp.com"},
		DenyIPs:      []string{"192.0.2.1"},
		Source:       "https://big.oisd.nl/domainswild",
		Refresh:      "12h",
	}
	require.NoError(t, s.SavePolicy(p))

	got, err := s.LoadPolicy("corp-egress")
	require.NoError(t, err)
	require.Equal(t, p, got)
}

func TestPolicyNameValidation(t *testing.T) {
	s := testStore(t)
	for _, name := range []string{"default", "Uppercase", "has space", "a/b", "../escape", ".dotlead", "-dashlead", ""} {
		err := s.SavePolicy(&Policy{Name: name})
		require.Error(t, err, "SavePolicy(%q) should be rejected", name)
	}
	for _, name := range []string{"oisd", "corp-egress", "a.b_c-1", "0zero"} {
		require.NoError(t, s.SavePolicy(&Policy{Name: name}), "SavePolicy(%q) should be accepted", name)
	}
}

func TestLoadPolicyNotFound(t *testing.T) {
	s := testStore(t)
	_, err := s.LoadPolicy("missing")
	require.ErrorIs(t, err, ErrPolicyNotFound)
}

func TestLoadPolicyBuiltinDefault(t *testing.T) {
	s := testStore(t)
	p, err := s.LoadPolicy("default")
	require.NoError(t, err)
	require.Equal(t, "default", p.Name)
	require.Equal(t, DefaultAllowedDomains, p.AllowDomains)
}

func TestListPolicies(t *testing.T) {
	s := testStore(t)

	// Empty store: no policies dir yet, and the builtin is never listed.
	got, err := s.ListPolicies()
	require.NoError(t, err)
	require.Empty(t, got)

	require.NoError(t, s.SavePolicy(&Policy{Name: "zeta"}))
	require.NoError(t, s.SavePolicy(&Policy{Name: "alpha"}))
	got, err = s.ListPolicies()
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "alpha", got[0].Name)
	require.Equal(t, "zeta", got[1].Name)
}

func TestPolicyCacheSaveLoad(t *testing.T) {
	s := testStore(t)
	require.NoError(t, s.SavePolicy(&Policy{Name: "oisd", Source: "https://example.com/list"}))

	// Absent cache reads as an empty one, never an error.
	c, err := s.LoadPolicyCache("oisd")
	require.NoError(t, err)
	require.NotNil(t, c)
	require.True(t, c.FetchedAt.IsZero())
	require.Empty(t, c.DenyDomains)

	want := &PolicyCache{
		FetchedAt:    time.Now().Truncate(time.Second).UTC(),
		ETag:         `"abc123"`,
		DenyDomains:  []string{"ads.example.com", "tracker.example.com"},
		AllowDomains: []string{"cdn.example.com"},
	}
	require.NoError(t, s.SavePolicyCache("oisd", want))
	c, err = s.LoadPolicyCache("oisd")
	require.NoError(t, err)
	require.Equal(t, want, c)
}

func TestDeletePolicy(t *testing.T) {
	s := testStore(t)
	require.NoError(t, s.SavePolicy(&Policy{Name: "doomed"}))
	require.NoError(t, s.SavePolicyCache("doomed", &PolicyCache{DenyDomains: []string{"x.com"}}))

	require.NoError(t, s.DeletePolicy("doomed"))
	_, err := s.LoadPolicy("doomed")
	require.ErrorIs(t, err, ErrPolicyNotFound)
	_, err = os.Stat(s.policyDir("doomed"))
	require.ErrorIs(t, err, os.ErrNotExist)

	// Deleting again is a no-op; deleting the builtin is rejected.
	require.NoError(t, s.DeletePolicy("doomed"))
	require.Error(t, s.DeletePolicy("default"))
}

func TestPolicyRefreshInterval(t *testing.T) {
	d, err := (&Policy{Name: "p"}).RefreshInterval()
	require.NoError(t, err)
	require.Equal(t, 24*time.Hour, d)

	d, err = (&Policy{Name: "p", Refresh: "90m"}).RefreshInterval()
	require.NoError(t, err)
	require.Equal(t, 90*time.Minute, d)

	_, err = (&Policy{Name: "p", Refresh: "soon"}).RefreshInterval()
	require.Error(t, err)
}

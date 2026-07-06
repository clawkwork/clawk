package netfilter

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAllowListStatic(t *testing.T) {
	al, err := NewAllowList(
		[]string{"1.2.3.4", "5.6.7.8"},
		[]string{"10.0.0.0/24", "192.168.10.0/24"},
		nil,
	)
	require.NoError(t, err)

	cases := []struct {
		addr  string
		allow bool
	}{
		{"1.2.3.4:80", true},
		{"5.6.7.8:443", true},
		{"10.0.0.5:22", true},       // in CIDR
		{"192.168.10.250:22", true}, // in CIDR
		{"192.168.11.1:22", false},  // outside CIDR
		{"8.8.8.8:53", false},
		{"evil.com:443", false}, // not literal IP
	}
	for _, c := range cases {
		err := al.Allow(c.addr)
		got := err == nil
		if got != c.allow {
			t.Errorf("Allow(%q) = %v (err=%v), want %v", c.addr, got, err, c.allow)
		}
	}
}

func TestSplitEntries(t *testing.T) {
	ips, cidrs := SplitEntries([]string{"1.2.3.4", "10.0.0.0/24", " ", "5.6.7.8", "192.168.0.0/16"})
	if len(ips) != 2 || ips[0] != "1.2.3.4" || ips[1] != "5.6.7.8" {
		t.Errorf("unexpected ips: %v", ips)
	}
	if len(cidrs) != 2 || cidrs[0] != "10.0.0.0/24" || cidrs[1] != "192.168.0.0/16" {
		t.Errorf("unexpected cidrs: %v", cidrs)
	}
}

func TestAllowListInvalidIP(t *testing.T) {
	_, err := NewAllowList([]string{"not-an-ip"}, nil, nil)
	require.Error(t, err, "expected error for non-IP entry")
}

func TestObserveDNSAllowsMatchingNames(t *testing.T) {
	al, err := NewAllowList(nil, nil, []string{"*.snapcraft.io", "api.example.com"})
	require.NoError(t, err)

	cases := []struct {
		name  string
		ip    string
		allow bool
	}{
		{"cdn.snapcraft.io.", "1.1.1.1", true},
		{"api.example.com", "2.2.2.2", true},
		{"evil.example.org", "3.3.3.3", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			al.ObserveDNSAnswer(c.name, net.ParseIP(c.ip))
			err := al.Allow(c.ip + ":443")
			if got := err == nil; got != c.allow {
				t.Errorf("Allow(%s) after ObserveDNS(%s) = %v (err=%v), want %v",
					c.ip, c.name, got, err, c.allow)
			}
		})
	}
}

func TestDenialLedgerAttribution(t *testing.T) {
	al, err := NewAllowList(nil, nil, nil)
	require.NoError(t, err)
	base := time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC)
	clock := base
	al.now = func() time.Time { return clock }

	var hooked []Denial
	al.OnDenial = func(d Denial) { hooked = append(hooked, d) }

	// Guest resolves a blocked name, then dials it twice.
	al.ObserveDNSAnswer("api.blocked.dev.", net.ParseIP("9.9.9.9"))
	if err := al.Allow("9.9.9.9:443"); err == nil {
		t.Fatal("expected denial")
	} else if !strings.Contains(err.Error(), "api.blocked.dev") {
		t.Errorf("denial error should name the host, got: %v", err)
	}
	clock = base.Add(time.Minute)
	if err := al.Allow("9.9.9.9:443"); err == nil {
		t.Fatal("expected denial")
	}
	// A dial to an IP the guest never resolved falls back to the literal.
	clock = base.Add(2 * time.Minute)
	if err := al.Allow("8.8.8.8:53"); err == nil {
		t.Fatal("expected denial")
	}

	got := al.Denials()
	want := []Denial{
		{
			Host: "8.8.8.8", IP: "8.8.8.8", Port: "53", Count: 1,
			FirstSeen: base.Add(2 * time.Minute), LastSeen: base.Add(2 * time.Minute),
		},
		{
			Host: "api.blocked.dev", IP: "9.9.9.9", Port: "443", Count: 2,
			FirstSeen: base, LastSeen: base.Add(time.Minute),
		},
	}
	require.Len(t, got, len(want), "Denials() returned wrong number of entries")
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Denials()[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if len(hooked) != 2 {
		t.Errorf("OnDenial fired %d times, want 2 (once per new host)", len(hooked))
	}
}

func TestDenialLedgerEviction(t *testing.T) {
	al, err := NewAllowList(nil, nil, nil)
	require.NoError(t, err)
	base := time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC)
	clock := base
	al.now = func() time.Time { return clock }

	for i := range maxDenialHosts + 1 {
		clock = base.Add(time.Duration(i) * time.Second)
		addr := fmt.Sprintf("10.1.%d.%d:443", i/256, i%256)
		if err := al.Allow(addr); err == nil {
			t.Fatalf("expected denial for %s", addr)
		}
	}

	got := al.Denials()
	require.Len(t, got, maxDenialHosts, "ledger holds wrong number of entries")
	// The first (oldest) host must have been evicted.
	for _, d := range got {
		if d.Host == "10.1.0.0" {
			t.Error("oldest denial should have been evicted")
		}
	}
}

func TestSetPolicyLiveChanges(t *testing.T) {
	al, err := NewAllowList([]string{"1.2.3.4"}, nil, []string{"*.good.dev"})
	require.NoError(t, err)

	// Wildcard subdomain admitted via DNS observation.
	al.ObserveDNSAnswer("api.good.dev", net.ParseIP("5.5.5.5"))
	if err := al.Allow("5.5.5.5:443"); err != nil {
		t.Fatalf("observed wildcard IP should be allowed: %v", err)
	}

	// Revoke the wildcard, add a new static IP.
	require.NoError(t, al.SetPolicy([]string{"7.7.7.7"}, nil, nil))
	if err := al.Allow("5.5.5.5:443"); err == nil {
		t.Error("observed IP should be revoked once its domain leaves the policy")
	}
	if err := al.Allow("1.2.3.4:443"); err == nil {
		t.Error("old static IP should be revoked")
	}
	if err := al.Allow("7.7.7.7:443"); err != nil {
		t.Errorf("new static IP should be allowed: %v", err)
	}

	// Re-allowing the domain makes future observations count again, and a
	// name observed while denied still attributes the earlier denial.
	require.NoError(t, al.SetPolicy(nil, nil, []string{"*.good.dev"}))
	al.ObserveDNSAnswer("api.good.dev", net.ParseIP("5.5.5.5"))
	if err := al.Allow("5.5.5.5:443"); err != nil {
		t.Errorf("re-observed IP should be allowed after domain restored: %v", err)
	}
}

func TestSetPolicyInvalidEntriesRejected(t *testing.T) {
	al, err := NewAllowList([]string{"1.2.3.4"}, nil, nil)
	require.NoError(t, err)
	require.Error(t, al.SetPolicy([]string{"not-an-ip"}, nil, nil), "expected error for non-IP entry")
	// Failed SetPolicy must leave the previous policy intact.
	if err := al.Allow("1.2.3.4:443"); err != nil {
		t.Errorf("policy should be unchanged after failed SetPolicy: %v", err)
	}
}

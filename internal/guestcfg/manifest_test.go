package guestcfg

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestWriteDiskRoundTrip decodes the written disk exactly the way
// clawk-init does — json.Decoder over the raw device content, trailing
// zero padding unread — and checks the manifest survives.
func TestWriteDiskRoundTrip(t *testing.T) {
	m := Manifest{
		Hostname: "box",
		Network: &Network{
			Interface: "eth0",
			Address:   "192.168.127.2/24",
			Gateway:   "192.168.127.1",
			DNS:       []string{"192.168.127.1"},
		},
		User:   &User{Name: "agent", UID: 501, GID: 20, Groups: []string{"kvm"}},
		Mounts: []Mount{{Tag: "wt", Path: "/home/agent/workspace/wt"}},
		Files: []File{{
			Path: "/etc/profile.d/99-clawk-env.sh", Mode: 0o644,
			Content: []byte("export FOO=bar\nbinary\x00bytes"),
		}},
		Services: []Service{{Name: "agent", Path: AgentPath}},
	}

	disk := filepath.Join(t.TempDir(), "guestcfg.img")
	require.NoError(t, WriteDisk(m, disk), "WriteDisk")

	data, err := os.ReadFile(disk)
	require.NoError(t, err)
	require.Equal(t, 0, len(data)%512, "disk size %d not sector-aligned", len(data))

	var got Manifest
	require.NoError(t, json.NewDecoder(bytes.NewReader(data)).Decode(&got), "decoding like clawk-init does")
	require.Equal(t, Version, got.Version, "WriteDisk must default Version")
	require.True(t, got.Hostname == m.Hostname && got.User.UID == 501 && got.Network.Address == m.Network.Address, "round-trip mismatch: %+v", got)
	require.True(t, bytes.Equal(got.Files[0].Content, m.Files[0].Content), "file content (with NUL bytes) did not survive the round trip")
}

package guestcfg

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMountNinePVSockPortAdditive verifies the field is a clean additive change:
// it round-trips, and a zero port is omitted from the wire so an older
// clawk-init (which decodes into a Mount without the field) sees exactly the
// pre-9p JSON — no Version bump, no forced sandbox recreation.
func TestMountNinePVSockPortAdditive(t *testing.T) {
	m := Manifest{
		Version: Version,
		Mounts: []Mount{
			{Tag: "go_modcache", Path: "/home/agent/go/pkg/mod", NinePVSockPort: 1100},
			{Tag: "claude_agents", Path: "/home/agent/.claude/agents"}, // virtio-fs only
		},
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := string(b)
	if strings.Contains(js, `"ninep_port":0`) {
		t.Errorf("zero port not omitted (old inits would see a new key): %s", js)
	}
	if !strings.Contains(js, `"ninep_port":1100`) {
		t.Errorf("non-zero port missing from wire: %s", js)
	}

	var got Manifest
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Mounts[0].NinePVSockPort != 1100 {
		t.Errorf("cache mount port = %d, want 1100", got.Mounts[0].NinePVSockPort)
	}
	if got.Mounts[1].NinePVSockPort != 0 {
		t.Errorf("virtio-fs mount port = %d, want 0", got.Mounts[1].NinePVSockPort)
	}
}

package machine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// SuspendMeta identifies what wrote a suspend-to-disk state, so a later
// boot can tell — before handing the bytes to the hypervisor — whether a
// restore even makes sense. Written beside the state files by the daemon
// at suspend, consulted by restoreOrStart at the next boot.
//
// Absence is not an error: states written by earlier clawks have no
// meta, and the hypervisor's own validation (both vz and firecracker
// refuse state they can't load, and the caller cold-boots on failure)
// remains the backstop. The meta exists to turn that late, cryptic
// refusal into an early, readable log line.
type SuspendMeta struct {
	// Backend is the machine backend that wrote the state ("vz",
	// "firecracker"). Restoring across backends can never work.
	Backend string `json:"backend"`

	// SpecFingerprint captures the Spec fields that shape the
	// guest-visible virtual hardware (see SuspendSpecFingerprint). A
	// clawk release that changes how it builds the VM changes this, and
	// the state is then not restorable.
	SpecFingerprint string `json:"spec_fingerprint"`

	// ClawkVersion is the writer's version string. Informational only —
	// restores are never gated on it, because most upgrades don't touch
	// the VM shape — but it makes "which release wrote this?" a cat away.
	ClawkVersion string `json:"clawk_version,omitempty"`
}

// SuspendSpecFingerprint reduces a Spec to the fields that shape the
// guest-visible virtual hardware. Deliberately a readable string, not a
// hash: when a restore is skipped over a mismatch, the log line shows
// what changed instead of two opaque digests.
func SuspendSpecFingerprint(s Spec) string {
	return fmt.Sprintf("vcpu=%d mem=%d memmax=%d disks=%d nets=%d shares=%d nested=%t",
		s.VCPU, s.MemoryMiB, s.MemoryMaxMiB, len(s.Disks), len(s.Net), len(s.Shares), s.NestedVirt)
}

// IncompatibleWith returns a human-readable reason when a state written
// under m cannot be restored by a boot described by want, or "" when the
// restore should be attempted. An empty want field skips that check.
func (m SuspendMeta) IncompatibleWith(want SuspendMeta) string {
	if want.Backend != "" && m.Backend != "" && m.Backend != want.Backend {
		return fmt.Sprintf("written by the %s backend, this boot uses %s", m.Backend, want.Backend)
	}
	if want.SpecFingerprint != "" && m.SpecFingerprint != "" && m.SpecFingerprint != want.SpecFingerprint {
		return fmt.Sprintf("VM shape changed: was [%s], now [%s] (written by clawk %s)",
			m.SpecFingerprint, want.SpecFingerprint, orUnknown(m.ClawkVersion))
	}
	return ""
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// suspendMetaFile sits beside the backend's state files in the suspend
// dir. Not listed in suspendStateFiles: meta alone is not restorable
// state.
const suspendMetaFile = "meta.json"

// WriteSuspendMeta records m beside the state files in dir. Best-effort
// by contract — callers log a failure and move on, because the state
// itself is already safely written.
func WriteSuspendMeta(dir string, m SuspendMeta) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, suspendMetaFile), append(b, '\n'), 0o644)
}

// ReadSuspendMeta loads the meta written next to a suspend state.
// ok=false means absent or unreadable — a pre-meta state; callers
// proceed to the restore attempt and let the hypervisor decide.
func ReadSuspendMeta(dir string) (SuspendMeta, bool) {
	b, err := os.ReadFile(filepath.Join(dir, suspendMetaFile))
	if err != nil {
		return SuspendMeta{}, false
	}
	var m SuspendMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return SuspendMeta{}, false
	}
	return m, true
}

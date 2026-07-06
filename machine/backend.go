package machine

import (
	"context"
	"fmt"
	"sync"
)

// Backend constructs Machines for a specific hypervisor. Backends register
// themselves at package-init via Register; callers look them up by name with
// Get.
type Backend interface {
	// Name is a stable, lowercase identifier ("firecracker", "vz").
	Name() string

	// Capabilities describes what the backend supports. Stable for the life
	// of the process.
	Capabilities() Caps

	// New prepares a Machine for the given Spec. stateDir is an
	// already-created directory that the Machine owns for the duration of its
	// life; the backend may place disks, sockets, pidfiles, and logs inside.
	//
	// New validates the Spec against the backend's capabilities and returns
	// an error before any side effects if the Spec is unsupported. A
	// successful New does not boot the VM; callers must call Machine.Create
	// and then Machine.Start.
	New(ctx context.Context, spec Spec, stateDir string) (Machine, error)
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Backend{}
)

// Register adds b to the package-wide backend registry. Typically called from
// a backend package's init. Panics on duplicate names.
func Register(b Backend) {
	registryMu.Lock()
	defer registryMu.Unlock()
	name := b.Name()
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("machine: backend %q already registered", name))
	}
	registry[name] = b
}

// Get returns the backend registered under name. Returns an error wrapping
// ErrNoBackend if no such backend is registered in this binary.
func Get(name string) (Backend, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	b, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNoBackend, name)
	}
	return b, nil
}

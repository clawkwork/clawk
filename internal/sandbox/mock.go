package sandbox

import "github.com/clawkwork/clawk/internal/config"

// MockProvider is a test double for the VM provider.
type MockProvider struct {
	Created   []string
	Started   []string
	Stopped   []string
	Destroyed []string
	Running   map[string]bool
}

func NewMockProvider() *MockProvider {
	return &MockProvider{Running: make(map[string]bool)}
}

func (m *MockProvider) Create(sb *config.Sandbox) error {
	m.Created = append(m.Created, sb.Name)
	return nil
}

func (m *MockProvider) Start(sb *config.Sandbox) error {
	m.Started = append(m.Started, sb.Name)
	m.Running[sb.Name] = true
	return nil
}

func (m *MockProvider) Stop(sb *config.Sandbox) error {
	m.Stopped = append(m.Stopped, sb.Name)
	delete(m.Running, sb.Name)
	return nil
}

func (m *MockProvider) Destroy(sb *config.Sandbox) error {
	m.Destroyed = append(m.Destroyed, sb.Name)
	delete(m.Running, sb.Name)
	return nil
}

func (m *MockProvider) Status(sb *config.Sandbox) (string, error) {
	if m.Running[sb.Name] {
		return "Running", nil
	}
	return "stopped", nil
}

// Shell implements ShellProvider for testing.
func (m *MockProvider) Shell(sb *config.Sandbox, workdir string) error {
	return nil
}

// Exec implements ShellProvider for testing.
func (m *MockProvider) Exec(sb *config.Sandbox, command ...string) error {
	return nil
}

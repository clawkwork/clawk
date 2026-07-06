//go:build !darwin && !linux

package usermode

import (
	"context"
	"errors"
	"os"

	"github.com/clawkwork/clawk/machine"
)

// ErrUnsupported is returned on platforms we haven't ported to. The
// implementation relies on AF_UNIX SOCK_DGRAM with MSG_PEEK|MSG_TRUNC, which
// is available on darwin and linux today.
var ErrUnsupported = errors.New("usermode: unsupported on this platform")

type Config struct {
	SockPath string
	Forwards []machine.PortForward
	Filter   machine.Filter
}

type Stack struct {
	VMSocket *os.File
	GuestIP  string
	GuestMAC string
}

func Start(_ Config) (*Stack, error)           { return nil, ErrUnsupported }
func (s *Stack) Serve(_ context.Context) error { return ErrUnsupported }
func (s *Stack) Close() error                  { return nil }

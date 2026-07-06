//go:build !darwin

package cli

import (
	"errors"

	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(&cobra.Command{
		Use:    "__vzd <sandbox>",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return errors.New("vz provider is only supported on macOS")
		},
	})
}

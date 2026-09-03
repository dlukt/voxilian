package cmd

import (
	"errors"

	"github.com/spf13/cobra"
)

// serveCmd runs the authoritative world server: gateway + sim + store.
// Full implementation lands across M3 (gateway) and M4 (sim).
var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the Voxilian world server",
	RunE: func(cmd *cobra.Command, args []string) error {
		return errors.New("voxilian serve: not implemented yet (see docs/implementation-plan.md M3/M4)")
	},
}

func init() {
	rootCmd.AddCommand(serveCmd)
}

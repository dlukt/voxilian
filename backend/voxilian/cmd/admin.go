package cmd

import (
	"errors"

	"github.com/spf13/cobra"
)

// adminCmd hosts operator commands (kick/ban/save-now/spawn/give).
// Full implementation lands in M11-T3; WS admin role per spec §10/§11.
var adminCmd = &cobra.Command{
	Use:   "admin",
	Short: "Operator commands (stub until M11-T3)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return errors.New("voxilian admin: not implemented yet (see docs/implementation-plan.md M11-T3)")
	},
}

func init() {
	rootCmd.AddCommand(adminCmd)
}

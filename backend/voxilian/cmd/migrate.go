package cmd

import (
	"errors"

	"github.com/spf13/cobra"
)

// migrateCmd applies PostgreSQL migrations. Real behavior (embedded
// goose migrations, one-shot container compatible) lands in M1-T8.
var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Apply database migrations (stub until M1-T8)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return errors.New("voxilian migrate: not implemented yet (see docs/implementation-plan.md M1-T8)")
	},
}

func init() {
	rootCmd.AddCommand(migrateCmd)
}

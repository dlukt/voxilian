package cmd

import (
	"errors"

	"github.com/spf13/cobra"
)

// seedCmd loads versioned prototype content (spells/skills/mobs/items)
// into the §8.2 catalog tables. Pipeline + validator land in M9-T1 on
// top of the M1-T6d registry API.
var seedCmd = &cobra.Command{
	Use:   "seed",
	Short: "Load prototype content (stub until M9-T1)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return errors.New("voxilian seed: not implemented yet (see docs/implementation-plan.md M9-T1)")
	},
}

func init() {
	rootCmd.AddCommand(seedCmd)
}

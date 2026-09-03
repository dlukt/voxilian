package cmd

import (
	"os"

	"github.com/spf13/cobra"
)

// rootCmd is the base command for the Voxilian authoritative game server.
// Backend working directory is backend/voxilian; see docs/implementation-plan.md.
var rootCmd = &cobra.Command{
	Use:   "voxilian",
	Short: "Voxilian authoritative game server",
	Long: `Voxilian authoritative game server (Go backend for the Godot voxel client).

Subcommands: serve (world server), migrate (database migrations),
admin (operations), seed (prototype content loading).
See docs/backend-spec.md for the architecture specification.`,
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}

func init() {
	// Persistent flags (config file, log level, etc.) are introduced
	// with the config loader in M0-T2.
}

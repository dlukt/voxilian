package cmd

import (
	"fmt"
	"sort"

	"github.com/dlukt/voxilian/internal/config"
	"github.com/dlukt/voxilian/internal/store"
	"github.com/spf13/cobra"
)

// migrateDSN loads config (defaults < file < VOX_*) and returns the
// effective PostgreSQL DSN. Callers must never print it.
func migrateDSN() (string, error) {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return "", err
	}
	return cfg.PGDSN, nil
}

func newMigrateCmd() *cobra.Command {
	migrateCmd := &cobra.Command{
		Use:   "migrate",
		Short: "Embedded PostgreSQL migrations",
	}
	migrateCmd.AddCommand(&cobra.Command{
		Use:   "up",
		Short: "Apply all pending embedded migrations",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dsn, err := migrateDSN()
			if err != nil {
				return err
			}
			m, err := store.OpenMigrator(cmd.Context(), dsn)
			if err != nil {
				return err
			}
			defer func() { _ = m.Close() }()
			return m.Up(cmd.Context())
		},
	})
	migrateCmd.AddCommand(&cobra.Command{
		Use:   "down",
		Short: "Roll back exactly one applied migration",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dsn, err := migrateDSN()
			if err != nil {
				return err
			}
			m, err := store.OpenMigrator(cmd.Context(), dsn)
			if err != nil {
				return err
			}
			defer func() { _ = m.Close() }()
			return m.Down(cmd.Context())
		},
	})
	migrateCmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show embedded migration state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dsn, err := migrateDSN()
			if err != nil {
				return err
			}
			m, err := store.OpenMigrator(cmd.Context(), dsn)
			if err != nil {
				return err
			}
			defer func() { _ = m.Close() }()
			rep, err := m.Status(cmd.Context())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "current=%d target=%d pending=%d\n", rep.Current, rep.Target, rep.Pending())
			entries := append([]store.MigrationStatus(nil), rep.Entries...)
			sort.Slice(entries, func(i, j int) bool { return entries[i].Version < entries[j].Version })
			for _, e := range entries {
				state := "pending"
				if e.Applied {
					state = "applied"
				}
				fmt.Fprintf(out, "version=%d state=%s path=%s\n", e.Version, state, e.Path)
			}
			return nil
		},
	})
	return migrateCmd
}

var migrateCmd = newMigrateCmd()

func init() {
	rootCmd.AddCommand(migrateCmd)
}

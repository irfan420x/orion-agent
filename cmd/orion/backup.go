package main

import (
	"fmt"
	"strings"

	"github.com/irfan/orion/internal/backup"
	"github.com/irfan/orion/internal/config"
)

// cmdBackup snapshots the Orion data dir (~/.orion) with rotation
// (F-A17-13, rc23): `orion backup` copies the whole data dir into a
// timestamped snapshot under <parent>/.orion-backups and prunes all
// but the newest DefaultKeep snapshots. It loads the config file
// directly (no provider construction), so it works even when the
// configured provider has no API key.
func cmdBackup(args []string) error {
	if len(args) > 0 && isHelpArg(args[0]) {
		fmt.Print(`Usage: orion backup

Snapshot the Orion data dir (~/.orion) into <parent>/.orion-backups
(newest 7 kept). Takes no arguments. Restore with:
  oriond --restore <snapshot>
`)
		return nil
	}
	if len(args) > 0 {
		return fmt.Errorf("backup: takes no arguments (got %q)", strings.Join(args, " "))
	}
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	cfg, err := config.Load(defaultPath)
	if err != nil {
		return err
	}
	dir := backup.DefaultDir(cfg.DataDir)
	path, err := backup.Snapshot(cfg.DataDir, dir)
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	if _, err := backup.Prune(dir, backup.DefaultKeep); err != nil {
		return fmt.Errorf("backup: prune: %w", err)
	}
	fmt.Printf("snapshot: %s\n", path)
	return nil
}

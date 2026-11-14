package main

import (
	"fmt"
	"os"
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
//
// F-A18-7: a corrupt config.json must not block a backup — the data
// dir is what is being saved, and the operator needs a backup most
// exactly when the config is damaged. On a Load error cmdBackup
// warns on stderr and falls back to config.Default() (data dir
// ~/.orion). A missing config already falls back to defaults inside
// Load; only a present-but-unreadable config reaches the fallback.
//
// F-A18-8: the snapshot is LIVE, not quiesced — see
// backup.Snapshot. Files are copied one by one while the daemon may
// be writing, so a snapshot may mix moments; the manifest says so.
func cmdBackup(args []string) error {
	if len(args) > 0 && isHelpArg(args[0]) {
		fmt.Print(`Usage: orion backup

Snapshot the Orion data dir (~/.orion) into <parent>/.orion-backups
(newest 7 kept). Takes no arguments. Restore with:
  oriond --restore <snapshot>

The snapshot is live, not quiesced: files are copied one by one
while the daemon may be writing, so a snapshot can mix moments
(memory.json from after a turn whose sessions.json predates it).
Each file's hash covers the bytes copied, but there is no
cross-file instant. The manifest records this (quiesced=false).
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
		// F-A18-7 (rc24): a corrupt config must not block a
		// backup. Fall back to the defaults (data dir
		// ~/.orion) and say so on stderr, so the fallback is
		// visible, never silent. The fallback only needs the
		// data dir — no provider is constructed here.
		//
		// F-A19-2 (rc25): the fallback must honour
		// ORION_DATA_DIR, the same env override Load applies
		// — a corrupt config must not make a backup silently
		// target ~/.orion while the daemon runs on another
		// dir. The warning names the dir actually used, and
		// where it came from, so the fallback is visible.
		cfg = config.Default()
		src := "default data dir"
		if dir := os.Getenv("ORION_DATA_DIR"); dir != "" {
			cfg.DataDir = dir
			src = "data dir from ORION_DATA_DIR"
		}
		fmt.Fprintf(os.Stderr, "backup: cannot load config %s (%v); the config file's data_dir is unknown, using %s %s\n", defaultPath, err, src, cfg.DataDir)
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

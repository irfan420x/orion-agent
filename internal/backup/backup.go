// Package backup snapshots the Orion data dir (~/.orion) with
// rotation and a verified restore path (F-A17-13, rc23).
//
// Before this package there was no backup of ~/.orion at all: one
// config file, one data dir, no .bak anywhere — a wipe of the data
// dir took config, memory, sessions, permission rules and budgets
// together, with nothing to restore from. Snapshot copies the
// whole data dir into a timestamped snapshot directory (default
// <parent-of-data-dir>/.orion-backups/orion-<timestamp>-<rand>,
// newest 7 kept) with a manifest.json listing every file's size
// and SHA-256. Restore verifies every hash BEFORE writing
// anything, so a corrupt snapshot is refused instead of
// half-restored, then copies the files back (overwriting what is
// there, adding what is missing; files in the data dir that are
// not in the snapshot are left alone).
//
// Symlinks are never followed or copied: a link inside the data
// dir pointing outside it must not pull outside files into a
// snapshot (or recreate the link on restore). Non-regular files
// (sockets, devices, fifos) are skipped the same way.
//
// WIRING (coordinator): the daemon never calls this on its own
// — snapshots are an operator action. Suggested entry points:
//
//	// cmd/oriond/main.go, flag handling (restore before the app starts):
//	restore := flag.String("restore", "", "restore ~/.orion from a backup snapshot and exit")
//	// ... after cfg.DataDir is known:
//	if *restore != "" {
//	    if err := backup.Restore(*restore, cfg.DataDir); err != nil { ...; return 1 }
//	    fmt.Fprintln(os.Stderr, "oriond: restored data dir from snapshot; start normally to run")
//	    return 0
//	}
//
//	// cmd/orion/main.go, a `orion backup` subcommand:
//	path, err := backup.Snapshot(cfg.DataDir, backup.DefaultDir(cfg.DataDir))
//	// + backup.Prune(dir, backup.DefaultKeep) for rotation.
//
// The snapshot format is versioned (manifest version 1); a
// future format bumps the version and Restore refuses files it
// does not understand rather than guessing.
package backup

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DefaultKeep is how many snapshots rotation keeps.
const DefaultKeep = 7

// manifestVersion is the only manifest version this code reads or
// writes.
const manifestVersion = 1

// manifestFileName is the manifest's file name inside a snapshot.
const manifestFileName = "manifest.json"

// DefaultDir returns the default backup root for a data dir: a
// sibling of the data dir, so a snapshot never lives inside the
// tree it backs up (a wipe of the data dir then leaves the
// snapshots alone).
func DefaultDir(dataDir string) string {
	return filepath.Join(filepath.Dir(dataDir), ".orion-backups")
}

// manifestFile is one file's entry in the manifest.
type manifestFile struct {
	Path   string      `json:"path"` // relative, slash-separated
	Size   int64       `json:"size"`
	SHA256 string      `json:"sha256"`
	Mode   fs.FileMode `json:"mode"`
}

// manifest is the snapshot's manifest.json.
type manifest struct {
	Version   int            `json:"version"`
	CreatedAt time.Time      `json:"created_at"`
	Source    string         `json:"source"`
	Files     []manifestFile `json:"files"`
}

// SnapshotInfo is one snapshot as List reports it.
type SnapshotInfo struct {
	Path      string
	CreatedAt time.Time
	Files     int
	TotalSize int64
}

// Snapshot copies dataDir into a new timestamped snapshot under
// backupDir and returns the snapshot's path. backupDir is created
// when missing. The snapshot name embeds the creation time plus
// 4 random hex chars, so two snapshots in the same second never
// collide. Rotation is NOT done here — call Prune after Snapshot
// (the CLI does) so a caller that snapshots in a loop cannot
// delete the snapshot it just made by mistake.
func Snapshot(dataDir, backupDir string) (string, error) {
	if strings.TrimSpace(dataDir) == "" {
		return "", fmt.Errorf("backup: data dir must not be empty")
	}
	if strings.TrimSpace(backupDir) == "" {
		return "", fmt.Errorf("backup: backup dir must not be empty")
	}
	srcAbs, err := filepath.Abs(dataDir)
	if err != nil {
		return "", fmt.Errorf("backup: resolve data dir: %w", err)
	}
	dstRootAbs, err := filepath.Abs(backupDir)
	if err != nil {
		return "", fmt.Errorf("backup: resolve backup dir: %w", err)
	}
	info, err := os.Stat(srcAbs)
	if err != nil {
		return "", fmt.Errorf("backup: stat data dir %q: %w", srcAbs, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("backup: data dir %q is not a directory", srcAbs)
	}
	var randBytes [2]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		return "", fmt.Errorf("backup: random snapshot suffix: %w", err)
	}
	name := fmt.Sprintf("orion-%s-%s", time.Now().Format("20060102-150405"), hex.EncodeToString(randBytes[:]))
	dst := filepath.Join(dstRootAbs, name)
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return "", fmt.Errorf("backup: create snapshot dir: %w", err)
	}
	// A snapshot that fails part-way is removed, never left
	// half-populated for Restore to trip over.
	man := manifest{Version: manifestVersion, CreatedAt: time.Now(), Source: srcAbs}
	walkErr := filepath.WalkDir(srcAbs, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Never copy the backup tree itself when it lives
		// inside the data dir (a caller passing a nested
		// backupDir): snapshots of snapshots grow without
		// bound and restore would resurrect them.
		if path == dstRootAbs || (d.IsDir() && strings.HasPrefix(path, dstRootAbs+string(filepath.Separator))) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(srcAbs, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if err := os.MkdirAll(filepath.Join(dst, rel), 0o700); err != nil {
				return fmt.Errorf("backup: create dir %q: %w", rel, err)
			}
			return nil
		}
		// Symlinks and non-regular files are skipped, never
		// followed: see the package comment.
		if d.Type()&fs.ModeSymlink != 0 || !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		sum, err := copyFileHashed(path, filepath.Join(dst, rel), fi.Mode())
		if err != nil {
			return err
		}
		man.Files = append(man.Files, manifestFile{
			Path:   filepath.ToSlash(rel),
			Size:   fi.Size(),
			SHA256: sum,
			Mode:   fi.Mode(),
		})
		return nil
	})
	if walkErr != nil {
		_ = os.RemoveAll(dst)
		return "", fmt.Errorf("backup: copy data dir: %w", walkErr)
	}
	manData, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		_ = os.RemoveAll(dst)
		return "", fmt.Errorf("backup: encode manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dst, manifestFileName), append(manData, '\n'), 0o600); err != nil {
		_ = os.RemoveAll(dst)
		return "", fmt.Errorf("backup: write manifest: %w", err)
	}
	return dst, nil
}

// copyFileHashed copies src to dst (creating parent dirs,
// preserving the mode) and returns the SHA-256 of the bytes
// copied. dst is written via a temp file + rename so a failed
// copy never leaves a partial file behind.
func copyFileHashed(src, dst string, mode fs.FileMode) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("backup: open %q: %w", src, err)
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", fmt.Errorf("backup: create dir for %q: %w", dst, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".backup-*.tmp")
	if err != nil {
		return "", fmt.Errorf("backup: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode.Perm()); err != nil {
		tmp.Close()
		return "", fmt.Errorf("backup: chmod temp file: %w", err)
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), in); err != nil {
		tmp.Close()
		return "", fmt.Errorf("backup: copy %q: %w", src, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", fmt.Errorf("backup: sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("backup: close temp file: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return "", fmt.Errorf("backup: rename temp file: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Restore copies a snapshot back into dataDir. Every file's hash
// is verified against the manifest BEFORE anything is written,
// so a corrupt snapshot is refused whole instead of restoring
// half the files and then failing. Restored files overwrite what
// is there and add what is missing; files in the data dir that
// are not in the snapshot are left alone (a merge restore, not a
// wipe-and-replace — deleting the operator's newer files because
// a snapshot predates them would be data loss, not recovery).
func Restore(snapshotPath, dataDir string) error {
	if strings.TrimSpace(snapshotPath) == "" {
		return fmt.Errorf("backup: snapshot path must not be empty")
	}
	if strings.TrimSpace(dataDir) == "" {
		return fmt.Errorf("backup: data dir must not be empty")
	}
	snapAbs, err := filepath.Abs(snapshotPath)
	if err != nil {
		return fmt.Errorf("backup: resolve snapshot path: %w", err)
	}
	manData, err := os.ReadFile(filepath.Join(snapAbs, manifestFileName))
	if err != nil {
		return fmt.Errorf("backup: read manifest in %q: %w", snapAbs, err)
	}
	var man manifest
	if err := json.Unmarshal(manData, &man); err != nil {
		return fmt.Errorf("backup: corrupt manifest in %q: %w", snapAbs, err)
	}
	if man.Version != manifestVersion {
		return fmt.Errorf("backup: unsupported manifest version %d in %q (want %d)", man.Version, snapAbs, manifestVersion)
	}
	// Verify first, write second: hash every snapshot file and
	// refuse the whole restore on the first mismatch.
	for _, f := range man.Files {
		if f.Path == "" || strings.Contains(f.Path, "..") || filepath.IsAbs(f.Path) {
			return fmt.Errorf("backup: manifest in %q has unsafe path %q", snapAbs, f.Path)
		}
		p := filepath.Join(snapAbs, filepath.FromSlash(f.Path))
		sum, err := hashFile(p)
		if err != nil {
			return fmt.Errorf("backup: verify %q: %w", f.Path, err)
		}
		if sum != f.SHA256 {
			return fmt.Errorf("backup: snapshot file %q is corrupt (hash mismatch); restore refused", f.Path)
		}
		fi, err := os.Stat(p)
		if err != nil {
			return fmt.Errorf("backup: stat %q: %w", f.Path, err)
		}
		if fi.Size() != f.Size {
			return fmt.Errorf("backup: snapshot file %q has size %d, manifest says %d; restore refused", f.Path, fi.Size(), f.Size)
		}
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("backup: create data dir: %w", err)
	}
	for _, f := range man.Files {
		src := filepath.Join(snapAbs, filepath.FromSlash(f.Path))
		dst := filepath.Join(dataDir, filepath.FromSlash(f.Path))
		if _, err := copyFileHashed(src, dst, f.Mode); err != nil {
			return fmt.Errorf("backup: restore %q: %w", f.Path, err)
		}
	}
	return nil
}

// hashFile returns the SHA-256 hex of a file's contents.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// List returns the snapshots under backupDir, newest first. A
// directory without a readable manifest is skipped, never fatal:
// a half-written snapshot (a crash mid-Snapshot leaves none —
// Snapshot removes it — but a hand-made directory is possible)
// must not hide the good snapshots.
func List(backupDir string) ([]SnapshotInfo, error) {
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("backup: list %q: %w", backupDir, err)
	}
	var out []SnapshotInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(backupDir, e.Name())
		manData, err := os.ReadFile(filepath.Join(p, manifestFileName))
		if err != nil {
			continue
		}
		var man manifest
		if err := json.Unmarshal(manData, &man); err != nil || man.Version != manifestVersion {
			continue
		}
		var total int64
		for _, f := range man.Files {
			total += f.Size
		}
		out = append(out, SnapshotInfo{Path: p, CreatedAt: man.CreatedAt, Files: len(man.Files), TotalSize: total})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].Path > out[j].Path
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

// Prune deletes all but the newest keep snapshots under
// backupDir and returns the removed paths. keep <= 0 keeps
// nothing. A snapshot that cannot be read as a snapshot (no
// valid manifest) is never deleted by Prune: it is not a
// snapshot this code owns.
func Prune(backupDir string, keep int) ([]string, error) {
	snaps, err := List(backupDir)
	if err != nil {
		return nil, err
	}
	if keep < 0 {
		keep = 0
	}
	if len(snaps) <= keep {
		return nil, nil
	}
	var removed []string
	for _, s := range snaps[keep:] {
		if err := os.RemoveAll(s.Path); err != nil {
			return removed, fmt.Errorf("backup: remove old snapshot %q: %w", s.Path, err)
		}
		removed = append(removed, s.Path)
	}
	return removed, nil
}

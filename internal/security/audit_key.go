// Audit-log HMAC key management.
//
// The v2 audit chain (see audit.go) is HMAC-SHA256 under a single
// persistent 32-byte key. The key used to live next to the log
// (audit.key in the log directory); since rc8 it lives in a
// SEPARATE directory — <log-dir>-keys/audit.key, e.g. ~/.orion-keys/
// audit.key for a log in ~/.orion (P2-O8). Co-locating the key with
// the log meant anyone who could read or write the log directory
// got the key that authenticates it; the separate directory (0700,
// key file 0600) means a log-directory compromise no longer yields
// the key. It is generated with crypto/rand the first time a log in
// that directory is opened and reused on every later open; the same
// key must verify every v2 entry, so a key file with the wrong
// length is an error and is never silently replaced (replacing the
// key would orphan the whole v2 chain). The key bytes are never
// logged, printed, or included in any error message.
//
// Migration: a key left at the old location (<log-dir>/audit.key)
// by an earlier build is MOVED — same bytes, new location, old file
// removed — the first time the log is opened, so the v2 chain keeps
// verifying under the same key and no upgrade silently orphans the
// old key. If keys exist at both locations with DIFFERENT bytes the
// open fails rather than guessing which one is authoritative. A
// leftover old-location key with identical bytes is removed
// best-effort (it is the same key, so a leftover copy is untidy
// rather than a security issue).
package security

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
)

// auditKeyFilename is the key file name inside the key directory.
const auditKeyFilename = "audit.key"

// auditKeyLen is the HMAC-SHA256 key length in bytes (256-bit key).
const auditKeyLen = 32

// auditKeyDir returns the directory holding the HMAC key for a log
// in dir: a sibling of the log directory, never the log directory
// itself (P2-O8).
func auditKeyDir(dir string) string {
	return dir + "-keys"
}

// auditKeyPath returns the path of the HMAC key for a log in dir.
func auditKeyPath(dir string) string {
	return filepath.Join(auditKeyDir(dir), auditKeyFilename)
}

// auditKeyLegacyPath returns the pre-rc8 key location
// (<log-dir>/audit.key), used only for migration.
func auditKeyLegacyPath(dir string) string {
	return filepath.Join(dir, auditKeyFilename)
}

// loadOrCreateAuditKey loads the 32-byte HMAC key for a log in dir,
// generating and storing a fresh crypto/rand key (mode 0600) when
// none exists. The key directory is created 0700 and the directory
// entry is fsynced so a newly created key survives a crash: losing
// the key would orphan every v2 entry already in the log. A
// pre-existing key file has its mode tightened to 0600 on every
// load. Concurrent first opens race safely: creation uses O_EXCL
// and the loser re-reads the winner's key instead of overwriting
// it. A key left at the legacy location is migrated (see
// migrateAuditKey); the O_EXCL key-creation race is unchanged.
func loadOrCreateAuditKey(dir string) ([]byte, error) {
	path := auditKeyPath(dir)
	data, err := os.ReadFile(path)
	if err == nil {
		if len(data) != auditKeyLen {
			return nil, fmt.Errorf("security: audit key %s has length %d, want %d", path, len(data), auditKeyLen)
		}
		// Enforce the mode on every load, not only on creation.
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, fmt.Errorf("security: chmod audit key %s: %w", path, err)
		}
		// A legacy key left behind by a crashed migration: the
		// new location is authoritative. Identical bytes are
		// the same key, so the leftover is removed best-effort;
		// different bytes mean someone planted a second key,
		// and the open fails rather than guessing.
		if legacy, lerr := os.ReadFile(auditKeyLegacyPath(dir)); lerr == nil {
			if !bytes.Equal(legacy, data) {
				return nil, fmt.Errorf("security: audit keys exist at both %s and %s with different values; refusing to guess which is authoritative", path, auditKeyLegacyPath(dir))
			}
			_ = os.Remove(auditKeyLegacyPath(dir))
		}
		return data, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("security: read audit key %s: %w", path, err)
	}

	// No key at the new location: migrate a legacy key if one
	// exists, so an upgrade keeps verifying the same v2 chain.
	if legacy, lerr := os.ReadFile(auditKeyLegacyPath(dir)); lerr == nil {
		if len(legacy) != auditKeyLen {
			return nil, fmt.Errorf("security: audit key %s has length %d, want %d", auditKeyLegacyPath(dir), len(legacy), auditKeyLen)
		}
		if err := migrateAuditKey(dir, legacy); err != nil {
			return nil, err
		}
		return legacy, nil
	} else if !os.IsNotExist(lerr) {
		return nil, fmt.Errorf("security: read audit key %s: %w", auditKeyLegacyPath(dir), lerr)
	}

	key := make([]byte, auditKeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("security: generate audit key: %w", err)
	}
	if err := os.MkdirAll(auditKeyDir(dir), 0o700); err != nil {
		return nil, fmt.Errorf("security: create audit key dir %s: %w", auditKeyDir(dir), err)
	}
	if err := os.Chmod(auditKeyDir(dir), 0o700); err != nil {
		return nil, fmt.Errorf("security: chmod audit key dir %s: %w", auditKeyDir(dir), err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			// Lost the creation race: the winner's key is the key.
			return loadOrCreateAuditKey(dir)
		}
		return nil, fmt.Errorf("security: create audit key %s: %w", path, err)
	}
	if _, err := f.Write(key); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("security: write audit key %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("security: fsync audit key %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("security: close audit key %s: %w", path, err)
	}
	if err := fsyncDir(auditKeyDir(dir)); err != nil {
		return nil, fmt.Errorf("security: fsync audit key dir %s: %w", auditKeyDir(dir), err)
	}
	return key, nil
}

// migrateAuditKey moves a legacy key (same bytes) to the new
// location: create the key directory (0700), write the key with
// O_EXCL (0600, fsynced), then remove the legacy file so only one
// copy remains. A concurrent migrator that wins the O_EXCL race is
// fine when it wrote the same key; a different key there is an
// error, not a key to overwrite.
func migrateAuditKey(dir string, legacy []byte) error {
	keyDir := auditKeyDir(dir)
	if err := os.MkdirAll(keyDir, 0o700); err != nil {
		return fmt.Errorf("security: create audit key dir %s: %w", keyDir, err)
	}
	if err := os.Chmod(keyDir, 0o700); err != nil {
		return fmt.Errorf("security: chmod audit key dir %s: %w", keyDir, err)
	}
	path := auditKeyPath(dir)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			winner, rerr := os.ReadFile(path)
			if rerr != nil {
				return fmt.Errorf("security: read migrated audit key %s: %w", path, rerr)
			}
			if !bytes.Equal(winner, legacy) {
				return fmt.Errorf("security: audit keys exist at both %s and %s with different values; refusing to guess which is authoritative", path, auditKeyLegacyPath(dir))
			}
			// Same key: another migrator already moved it.
		} else {
			return fmt.Errorf("security: create audit key %s: %w", path, err)
		}
	} else {
		if _, err := f.Write(legacy); err != nil {
			_ = f.Close()
			return fmt.Errorf("security: write audit key %s: %w", path, err)
		}
		if err := f.Sync(); err != nil {
			_ = f.Close()
			return fmt.Errorf("security: fsync audit key %s: %w", path, err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("security: close audit key %s: %w", path, err)
		}
		if err := fsyncDir(keyDir); err != nil {
			return fmt.Errorf("security: fsync audit key dir %s: %w", keyDir, err)
		}
	}
	// The key now lives at the new location: remove the legacy
	// file so the key no longer sits next to the log. A removal
	// failure is an error — a leftover legacy copy would defeat
	// the relocation — except that the file is already gone.
	if err := os.Remove(auditKeyLegacyPath(dir)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("security: remove legacy audit key %s: %w", auditKeyLegacyPath(dir), err)
	}
	if err := fsyncDir(dir); err != nil {
		return fmt.Errorf("security: fsync audit key dir %s: %w", dir, err)
	}
	return nil
}

// fsyncDir fsyncs a directory so a newly created or renamed file in
// it is durable across a crash.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

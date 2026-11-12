// Audit-log HMAC key management.
//
// The v2 audit chain (see audit.go) is HMAC-SHA256 under a single
// persistent 32-byte key. The key lives in the same directory as the
// log file — in production the log is <data-dir>/audit.jsonl and the
// data dir defaults to ~/.orion, so the key is ~/.orion/audit.key —
// with mode 0600 inside the 0700 log directory. It is generated with
// crypto/rand the first time a log in that directory is opened and
// reused on every later open; the same key must verify every v2
// entry, so a key file with the wrong length is an error and is never
// silently replaced (replacing the key would orphan the whole v2
// chain). The key bytes are never logged, printed, or included in
// any error message.
package security

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
)

// auditKeyFilename is the key file name inside the log directory.
const auditKeyFilename = "audit.key"

// auditKeyLen is the HMAC-SHA256 key length in bytes (256-bit key).
const auditKeyLen = 32

// auditKeyPath returns the path of the HMAC key for a log in dir.
func auditKeyPath(dir string) string {
	return filepath.Join(dir, auditKeyFilename)
}

// loadOrCreateAuditKey loads the 32-byte HMAC key at
// dir/audit.key, generating and storing a fresh crypto/rand key
// (mode 0600) when none exists. The directory entry is fsynced so a
// newly created key survives a crash: losing the key would orphan
// every v2 entry already in the log. A pre-existing key file has its
// mode tightened to 0600 on every load. Concurrent first opens race
// safely: creation uses O_EXCL and the loser re-reads the winner's
// key instead of overwriting it.
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
		return data, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("security: read audit key %s: %w", path, err)
	}

	key := make([]byte, auditKeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("security: generate audit key: %w", err)
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
	if err := fsyncDir(dir); err != nil {
		return nil, fmt.Errorf("security: fsync audit key dir %s: %w", dir, err)
	}
	return key, nil
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

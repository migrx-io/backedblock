package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeMgxCLI has mgx-cli's shape: a cmd.Cmd whose login takes a user and
// --cluster/--ns, started by main().
const fakeMgxCLI = `#!/usr/bin/env python3
import shlex, argparse
from cmd import Cmd
class Cli(Cmd):
    def __init__(self):
        Cmd.__init__(self)
        self.prompt = "mgx> "
        self.intro = "Migrx CLI"
    def do_login(self, line):
        p = argparse.ArgumentParser()
        p.add_argument("username")
        p.add_argument("--cluster")
        p.add_argument("--ns")
        a = p.parse_args(shlex.split(line))
        print("LOGIN", repr(a.username), repr(a.cluster), repr(a.ns))
    def do_quit(self, _):
        return True
def main():
    Cli().cmdloop()
`

func TestMgxCLICommand(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "mgx-cli")
	if err := os.WriteFile(fake, []byte(fakeMgxCLI), 0o755); err != nil {
		t.Fatal(err)
	}
	cluster := `it's "main" $(x)`
	sh := strings.ReplaceAll(mgxCLICommand("admin", cluster, "password: bbctl secrets get dev MGX_GW_ADMIN_PASSWD"), "/usr/bin/mgx-cli", fake)
	c := exec.Command("sh", "-c", sh)
	c.Stdin = strings.NewReader("quit\n")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{
		"Migrx CLI\npassword: bbctl secrets get dev MGX_GW_ADMIN_PASSWD\nmgx> login admin --cluster",
		`LOGIN 'admin' 'it\'s "main" $(x)' 'it\'s "main" $(x)'`,
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

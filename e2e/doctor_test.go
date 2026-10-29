//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func (e *env) doctor(args ...string) (int, string) {
	e.t.Helper()
	args = append([]string{"doctor"}, args...)
	return e.runCmd(e.commandNoSource(args...), args)
}

func wantLines(t *testing.T, out string, lines ...string) {
	t.Helper()
	for _, l := range lines {
		if !strings.Contains(out, l) {
			t.Errorf("output lacks %q", l)
		}
	}
}

func TestDoctor(t *testing.T) {
	e := newEnv(t, envOpt{remoteExtra: "file_mode = 0o640\ndir_mode = 0o750"})

	// dest does not exist yet: only a warning, and nothing is written.
	code, out := e.doctor()
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	wantLines(t, out,
		"OK    TCP connection: ",
		"(in known_hosts)",
		"OK    login: key file "+server.keyPath+" (ssh-ed25519 ",
		"OK    SFTP",
		"WARN  dest: does not exist yet",
		"SKIP  write access: not checked; use --write-test",
	)

	code, out = e.doctor("--write-test")
	if code != 0 {
		t.Fatalf("--write-test: code %d", code)
	}
	wantLines(t, out,
		"OK    write access: wrote and removed a probe file in /photo\n",
		"OK    sha256sum on the NAS: matches the probe file",
		"OK    file_mode: 0o640 is applied",
		"OK    dir_mode: 0o750 is applied",
	)
	entries, err := os.ReadDir(server.photoDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range entries {
		if strings.HasPrefix(d.Name(), ".camback-probe-") {
			t.Errorf("probe left behind: %s", d.Name())
		}
	}

	// After a transfer, sha256sum is checked against a file in dest.
	if code, _ := e.run("--yes"); code != 0 {
		t.Fatalf("run: code %d", code)
	}
	code, out = e.doctor()
	if code != 0 {
		t.Fatalf("after run: code %d", code)
	}
	wantLines(t, out, "OK    dest: exists", "OK    sha256sum on the NAS: matches a download of /photo/"+e.dest+"/")
	e.noLeftovers()
}

func TestDoctorWithoutShellPathMap(t *testing.T) {
	e := newEnv(t, envOpt{noShellPathMap: true})
	code, out := e.doctor("--write-test")
	if code != 0 {
		t.Fatalf("a missing shell_path_map is a warning: code %d", code)
	}
	wantLines(t, out, "WARN  sha256sum on the NAS: ", "fix: check shell_path_map")
	e.noLeftovers()
}

func TestDoctorUnknownHostKeyWithoutTerminal(t *testing.T) {
	e := newEnv(t, envOpt{})
	if err := os.WriteFile(filepath.Join(e.home, ".ssh", "known_hosts"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := e.doctor()
	if code != 1 {
		t.Fatalf("code %d", code)
	}
	wantLines(t, out, "FAIL  host key: ", "fix: run camback doctor in a terminal")
}

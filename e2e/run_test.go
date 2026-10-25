//go:build e2e

package e2e

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	psftp "github.com/pkg/sftp"

	"github.com/TatsuyaOGth/camback/internal/testutil"
)

const (
	camMake  = "RICOH IMAGING COMPANY, LTD."
	camModel = "RICOH GR IIIx"
)

// base is the cards' modification time.
var base = time.Date(2026, 10, 16, 9, 26, 27, 0, time.FixedZone("JST", 9*60*60))

// env is one test's card, config and destination (/photo/<dest>).
type env struct {
	t    *testing.T
	dest string
	card string
	cfg  string
	home string
}

type envOpt struct {
	noShellPathMap bool
}

func newEnv(t *testing.T, o envOpt) *env {
	t.Helper()
	dir := t.TempDir()
	e := &env{
		t:    t,
		dest: strings.NewReplacer("/", "_").Replace(t.Name()),
		card: filepath.Join(dir, "card"),
		cfg:  filepath.Join(dir, "config.toml"),
		home: filepath.Join(dir, "home"),
	}
	mapLine := `shell_path_map = { "/photo" = "/volume1/photo" }`
	if o.noShellPathMap {
		mapLine = ""
	}
	cfg := fmt.Sprintf(`
[remote.nas]
host = %q
port = %s
user = "nas"
identity_file = %q
%s

[[camera]]
id = "GR3x"
match = [{ make = %q, model = %q }]
remote = "nas"
dest = "/photo/%s"
`, server.host, server.port, server.keyPath, mapLine, camMake, camModel, e.dest)
	if err := os.WriteFile(e.cfg, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(e.home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.home, ".ssh", "known_hosts"), []byte(server.knownHosts), 0o600); err != nil {
		t.Fatal(err)
	}
	testutil.WriteJPEG(t, filepath.Join(e.card, "DCIM", "100RICOH", "R0001.JPG"), camMake, camModel)
	os.Chtimes(filepath.Join(e.card, "DCIM", "100RICOH", "R0001.JPG"), base, base)
	return e
}

// writeCard writes a file on the card with the given modification time.
func (e *env) writeCard(rel, content string, mtime time.Time) {
	e.t.Helper()
	p := filepath.Join(e.card, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		e.t.Fatal(err)
	}
}

// nas is the local path of a file under the destination.
func (e *env) nas(rel string) string {
	return filepath.Join(server.photoDir, e.dest, filepath.FromSlash(rel))
}

func (e *env) readNAS(rel string) string {
	e.t.Helper()
	b, err := os.ReadFile(e.nas(rel))
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

func (e *env) command(args ...string) *exec.Cmd {
	cmd := exec.Command(bin, append([]string{"--config", e.cfg, "--source", e.card}, args...)...)
	cmd.Env = append(os.Environ(),
		"HOME="+e.home,
		"XDG_CONFIG_HOME="+filepath.Join(e.home, ".config"),
		"XDG_STATE_HOME="+filepath.Join(e.home, ".local", "state"),
		"SSH_AUTH_SOCK=", // keys come from identity_file only
		"CAMBACK_CONFIG=",
	)
	return cmd // stdin is /dev/null: any prompt fails instead of blocking
}

// run runs camback and returns its exit code and combined output.
func (e *env) run(args ...string) (int, string) {
	e.t.Helper()
	cmd := e.command(args...)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		e.t.Fatal(err)
	}
	e.t.Logf("camback %s -> %d\n%s", strings.Join(args, " "), cmd.ProcessState.ExitCode(), out)
	return cmd.ProcessState.ExitCode(), string(out)
}

// noLeftovers fails if a temporary or probe file is left in the destination.
func (e *env) noLeftovers() {
	e.t.Helper()
	filepath.WalkDir(filepath.Join(server.photoDir, e.dest), func(p string, d fs.DirEntry, err error) error {
		if err == nil && strings.Contains(d.Name(), ".camback-") {
			e.t.Errorf("leftover file %s", p)
		}
		return nil
	})
}

func TestTransferAndRerun(t *testing.T) {
	e := newEnv(t, envOpt{})
	e.writeCard("DCIM/100RICOH/R0001.DNG", "raw-1", base)
	e.writeCard("MISC/MEDIAPRO.XML", "v1", base)

	code, out := e.run("--yes")
	if code != 0 || !strings.Contains(out, "Done: 3 files") || !strings.Contains(out, "verified by sha256sum on destination") {
		t.Fatalf("first run: code %d", code)
	}
	for _, rel := range []string{"DCIM/100RICOH/R0001.JPG", "DCIM/100RICOH/R0001.DNG", "MISC/MEDIAPRO.XML"} {
		want, _ := os.ReadFile(filepath.Join(e.card, filepath.FromSlash(rel)))
		if got := e.readNAS(rel); got != string(want) {
			t.Errorf("%s: content differs", rel)
		}
		fi, err := os.Stat(e.nas(rel))
		if err != nil {
			t.Fatal(err)
		}
		if !fi.ModTime().Equal(base) {
			t.Errorf("%s: mtime %v, want %v", rel, fi.ModTime(), base)
		}
	}
	e.noLeftovers()

	if code, out := e.run("--yes"); code != 0 || !strings.Contains(out, "Nothing to transfer") {
		t.Fatalf("rerun: code %d", code)
	}
}

func TestConflictAbortsWithoutTransfer(t *testing.T) {
	e := newEnv(t, envOpt{})
	e.writeCard("DCIM/100RICOH/R0001.DNG", "raw-1", base)
	if code, _ := e.run("--yes"); code != 0 {
		t.Fatalf("first run: code %d", code)
	}

	// A different photo with the same name and size is on the NAS.
	if err := os.WriteFile(e.nas("DCIM/100RICOH/R0001.DNG"), []byte("raw-2"), 0o644); err != nil {
		t.Fatal(err)
	}
	later := base.Add(time.Hour)
	os.Chtimes(e.nas("DCIM/100RICOH/R0001.DNG"), later, later)
	e.writeCard("DCIM/100RICOH/R0002.DNG", "new", base)

	code, out := e.run("--yes")
	if code != 2 || !strings.Contains(out, "CONFLICT") || !strings.Contains(out, "nothing was transferred") {
		t.Fatalf("code %d, want 2", code)
	}
	if got := e.readNAS("DCIM/100RICOH/R0001.DNG"); got != "raw-2" {
		t.Errorf("conflicting file changed: %q", got)
	}
	if _, err := os.Stat(e.nas("DCIM/100RICOH/R0002.DNG")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("new file was transferred despite the conflict: %v", err)
	}
	e.noLeftovers()
}

func TestChangedFileIsSavedUnderNewName(t *testing.T) {
	e := newEnv(t, envOpt{})
	e.writeCard("MISC/MEDIAPRO.XML", "v1", base)
	if code, _ := e.run("--yes"); code != 0 {
		t.Fatalf("first run: code %d", code)
	}

	e.writeCard("MISC/MEDIAPRO.XML", "version 2", base.Add(time.Hour))
	code, out := e.run("--yes")
	if code != 0 || !strings.Contains(out, "RENAME") {
		t.Fatalf("second run: code %d", code)
	}
	if got := e.readNAS("MISC/MEDIAPRO.XML"); got != "v1" {
		t.Errorf("original changed: %q", got)
	}
	// 10:26:27 JST = 01:26:27 UTC
	if got := e.readNAS("MISC/MEDIAPRO_20261016-012627Z.XML"); got != "version 2" {
		t.Errorf("renamed copy: %q", got)
	}
	e.noLeftovers()

	if code, out := e.run("--yes"); code != 0 || !strings.Contains(out, "Nothing to transfer") {
		t.Fatalf("rerun: code %d", code)
	}
}

func TestVerifyMethods(t *testing.T) {
	cases := []struct {
		name string
		opt  envOpt
		args []string
		want []string
	}{
		{"sha256sum through shell_path_map", envOpt{}, nil,
			[]string{"verified by sha256sum on destination"}},
		{"read-back without shell_path_map", envOpt{noShellPathMap: true}, nil,
			[]string{"check shell_path_map", "verified by read-back"}},
		{"size", envOpt{}, []string{"--verify", "size"},
			[]string{"verified by size"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, c.opt)
			e.writeCard("DCIM/100RICOH/R0001.DNG", "raw-1", base)
			code, out := e.run(append([]string{"--yes"}, c.args...)...)
			if code != 0 {
				t.Fatalf("code %d", code)
			}
			for _, w := range c.want {
				if !strings.Contains(out, w) {
					t.Errorf("output lacks %q", w)
				}
			}
			if got := e.readNAS("DCIM/100RICOH/R0001.DNG"); got != "raw-1" {
				t.Errorf("content %q", got)
			}
			e.noLeftovers()
		})
	}
}

// camback never overwrites: it checks the target before renaming, and
// OpenSSH's sftp-server refuses a rename onto an existing file as well, so
// a file created in between is not overwritten either. The in-process test
// server overwrites, so this is only checked here.
func TestServerRefusesRenameOntoExistingFile(t *testing.T) {
	c, err := dialSSH()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fsc, err := psftp.NewClient(c)
	if err != nil {
		t.Fatal(err)
	}
	defer fsc.Close()

	dir := "/photo/" + t.Name()
	if err := fsc.MkdirAll(dir); err != nil {
		t.Fatal(err)
	}
	put := func(name, content string) {
		f, err := fsc.Create(dir + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(f, content)
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	put("from", "new")
	put("to", "old")
	if err := fsc.Rename(dir+"/from", dir+"/to"); err == nil {
		t.Fatal("rename onto an existing file succeeded")
	}
	if b, _ := os.ReadFile(filepath.Join(server.photoDir, t.Name(), "to")); string(b) != "old" {
		t.Errorf("target overwritten: %q", b)
	}
}

func TestInterruptRemovesPartialFile(t *testing.T) {
	e := newEnv(t, envOpt{})
	// Large enough that the transfer is still running when we interrupt it;
	// sparse on the card, so only the NAS side takes disk space.
	const size = 512 << 20
	big := filepath.Join(e.card, "PRIVATE", "BIG.BIN")
	os.MkdirAll(filepath.Dir(big), 0o755)
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	f.Close()
	os.Chtimes(big, base, base)

	var out bytes.Buffer
	cmd := e.command("--yes")
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	part := e.nas("PRIVATE/.BIG.BIN.camback-part")
	deadline := time.After(time.Minute)
wait:
	for {
		select {
		case <-done:
			t.Fatalf("camback finished before it could be interrupted\n%s", out.String())
		case <-deadline:
			cmd.Process.Kill()
			t.Fatalf("no partial file appeared\n%s", out.String())
		case <-time.After(2 * time.Millisecond):
			if _, err := os.Stat(part); err == nil {
				break wait
			}
		}
	}
	cmd.Process.Signal(os.Interrupt)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("camback did not stop after Ctrl-C\n%s", out.String())
	}
	t.Logf("interrupted run -> %d\n%s", cmd.ProcessState.ExitCode(), out.String())
	if code := cmd.ProcessState.ExitCode(); code != 3 || !strings.Contains(out.String(), "interrupted") {
		t.Fatalf("code %d, want 3", code)
	}
	if _, err := os.Stat(e.nas("PRIVATE/BIG.BIN")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("interrupted file exists under its final name: %v", err)
	}
	e.noLeftovers()

	// The next run transfers it completely.
	if code, out := e.run("--yes"); code != 0 || !strings.Contains(out, "Done:") {
		t.Fatalf("rerun: code %d", code)
	}
	if fi, err := os.Stat(e.nas("PRIVATE/BIG.BIN")); err != nil || fi.Size() != size {
		t.Errorf("after rerun: %v, %v", fi, err)
	}
	e.noLeftovers()
}

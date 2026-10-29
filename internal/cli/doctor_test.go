package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/TatsuyaOGth/camback/internal/config"
	"github.com/TatsuyaOGth/camback/internal/transport"
	"github.com/TatsuyaOGth/camback/internal/transport/sftp"
)

func newTestHostKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// newDoctorEnv is newEnv with a dial that fills in a successful trace.
func newDoctorEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	hostKey := newTestHostKey(t)
	old := dialTraceFunc
	dialTraceFunc = func(_ context.Context, _ config.Remote, _ sftp.Prompter, tr *sftp.Trace) (transport.Transport, error) {
		tr.Addrs = []string{"192.0.2.1"}
		tr.Remote = "192.0.2.1:22"
		tr.HostKey = hostKey
		tr.Auth = "ssh-agent key (ssh-ed25519 SHA256:test)"
		return e.nas, nil
	}
	t.Cleanup(func() { dialTraceFunc = old })
	return e
}

func (e *env) doctor(args ...string) (int, string) {
	var out bytes.Buffer
	args = append([]string{"--config", e.cfg, "doctor"}, args...)
	code := executeWith(context.Background(), args, e.stdin, &out, &out)
	return code, out.String()
}

func wantLines(t *testing.T, out string, lines ...string) {
	t.Helper()
	for _, l := range lines {
		if !strings.Contains(out, l) {
			t.Errorf("output lacks %q:\n%s", l, out)
		}
	}
}

func TestDoctorReadOnly(t *testing.T) {
	e := newDoctorEnv(t)
	e.nas.Put("/photo/GR3x/DCIM/100RICOH/R0001.DNG", []byte("raw"), time.Now())
	e.nas.Put("/photo/GR3x/DCIM/100RICOH/R0002.DNG", []byte("bigger raw"), time.Now())
	// Smaller, but made by the NAS (Synology thumbnails), not by a run.
	e.nas.Put("/photo/GR3x/@eaDir/x", []byte("t"), time.Now())
	e.nas.Put("/photo/GR3x/DCIM/100RICOH/@eaDir/R0001.DNG/SYNOFILE_THUMB_S.jpg", []byte("t"), time.Now())
	e.nas.Put("/photo/GR3x/#recycle/y", []byte("t"), time.Now())

	code, out := e.doctor()
	if code != ExitOK {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	wantLines(t, out,
		"OK    exiftool: ",
		"OK    config file: "+e.cfg+" (--config); 2 cameras, 1 remote",
		"OK    name lookup: 192.0.2.1",
		"OK    TCP connection: 192.0.2.1:22",
		" (in known_hosts)",
		"OK    login: ssh-agent key",
		"OK    SFTP",
		"Camera \"GR3x\" (nas:/photo/GR3x)\n  OK    dest: exists",
		// The smallest file a run could have written is downloaded.
		"OK    sha256sum on the NAS: matches a download of /photo/GR3x/DCIM/100RICOH/R0001.DNG",
		"SKIP  write access: not checked; use --write-test",
		"Camera \"XE4\" (nas:/photo/X-E4)\n  WARN  dest: does not exist yet",
		"No problems, 1 warning.",
	)
	if n := len(e.nas.Paths()); n != 5 {
		t.Errorf("doctor without --write-test wrote to the NAS: %v", e.nas.Paths())
	}
}

func TestDoctorWrongShellPathMap(t *testing.T) {
	e := newDoctorEnv(t)
	e.nas.WrongHash = true
	e.nas.Put("/photo/GR3x/R0001.DNG", []byte("raw"), time.Now())
	code, out := e.doctor()
	if code != ExitOK {
		t.Fatalf("a wrong shell_path_map is a warning, got exit %d:\n%s", code, out)
	}
	wantLines(t, out, "WARN  sha256sum on the NAS: hashed a different file", "fix: check shell_path_map")
}

func TestDoctorWriteTest(t *testing.T) {
	e := newDoctorEnv(t)
	os.WriteFile(e.cfg, []byte(strings.Replace(testConfig, `user = "me"`, "user = \"me\"\nfile_mode = 0o644\ndir_mode = 0o755", 1)), 0o644)
	e.nas.MkdirAll("/photo/GR3x")

	code, out := e.doctor("--write-test")
	if code != ExitOK {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	wantLines(t, out,
		"OK    write access: can write in /photo/GR3x",
		"OK    sha256sum on the NAS: matches the probe file",
		"OK    file_mode: 0o644 is applied",
		"OK    dir_mode: 0o755 is applied",
		// X-E4 does not exist: the probe goes into /photo.
		"OK    write access: can write in /photo\n",
	)
	if p := e.nas.Paths(); len(p) != 0 {
		t.Errorf("probe files left behind: %v", p)
	}
	if fi, err := e.nas.Stat("/photo/X-E4"); err == nil {
		t.Errorf("doctor created dest: %+v", fi)
	}
	entries, _ := e.nas.ReadDir("/photo")
	for _, fi := range entries {
		if strings.HasPrefix(fi.Name, ".camback-probe-") {
			t.Errorf("probe folder left behind: %s", fi.Name)
		}
	}
}

func TestDoctorModeNotApplied(t *testing.T) {
	e := newDoctorEnv(t)
	os.WriteFile(e.cfg, []byte(strings.Replace(testConfig, `user = "me"`, "user = \"me\"\nfile_mode = 0o644", 1)), 0o644)
	e.nas.MkdirAll("/photo/GR3x")
	e.nas.IgnoreChmod = true

	code, out := e.doctor("--write-test")
	if code != ExitFailure {
		t.Fatalf("exit %d, want %d:\n%s", code, ExitFailure, out)
	}
	wantLines(t, out, "FAIL  file_mode: chmod ", "does not apply permissions", "Error: 2 problems")
}

func TestDoctorConnectionFails(t *testing.T) {
	e := newDoctorEnv(t)
	dialTraceFunc = func(_ context.Context, _ config.Remote, p sftp.Prompter, tr *sftp.Trace) (transport.Transport, error) {
		p.Warn("known_hosts line 3 is ignored because it cannot be parsed (bad key)")
		tr.Addrs = []string{"192.0.2.1"}
		tr.Remote = "192.0.2.1:22"
		tr.Failed = sftp.StageAuth
		return nil, errors.New("ssh me@nas.test:22: unable to authenticate")
	}

	code, out := e.doctor()
	if code != ExitFailure {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	wantLines(t, out,
		"WARN  known_hosts: known_hosts line 3 is ignored",
		"OK    TCP connection: 192.0.2.1:22",
		"FAIL  login: ssh me@nas.test:22: unable to authenticate",
		`fix: check user = "me"`,
		`SKIP  camera "GR3x": not checked: cannot connect`,
	)
	// The stages after the failed one are not shown.
	if strings.Contains(out, "SFTP") || strings.Contains(out, "host key") {
		t.Errorf("stages after the failure are shown:\n%s", out)
	}
}

func TestDoctorConfigProblems(t *testing.T) {
	e := newDoctorEnv(t)
	dialTraceFunc = func(context.Context, config.Remote, sftp.Prompter, *sftp.Trace) (transport.Transport, error) {
		t.Error("no remote is checked without a config")
		return nil, errors.New("unreachable")
	}

	missing := filepath.Join(t.TempDir(), "none.toml")
	e.cfg = missing
	code, out := e.doctor()
	if code != ExitFailure {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	wantLines(t, out, "FAIL  config file: "+missing+" (--config) does not exist", "OK    exiftool: ", "Memory cards")

	// A folder, not a file: it cannot be read at all.
	e.cfg = t.TempDir()
	_, out = e.doctor()
	wantLines(t, out, "FAIL  config file: cannot read "+e.cfg+" (--config): ", "fix: check that it is a file you may read")
	if strings.Contains(out, "is not valid") {
		t.Errorf("a read error is not a validation error:\n%s", out)
	}

	e.cfg = missing
	os.WriteFile(missing, []byte("[defaults]\njobs = 0\n[[camera]]\nid = \"a\"\n"), 0o644)
	_, out = e.doctor()
	wantLines(t, out, "is not valid:", "defaults.jobs: must be between 1 and 8", `camera "a": remote "" is not defined`)
}

func TestDoctorFailsBeforeConnecting(t *testing.T) {
	e := newDoctorEnv(t)
	dialTraceFunc = func(_ context.Context, _ config.Remote, _ sftp.Prompter, tr *sftp.Trace) (transport.Transport, error) {
		// An unreadable identity_file stops Dial before it connects.
		tr.Addrs = []string{"192.0.2.1"}
		tr.Failed = sftp.StageAuth
		return nil, errors.New("identity_file: open /nowhere: no such file or directory")
	}
	code, out := e.doctor()
	if code != ExitFailure {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	wantLines(t, out, "OK    name lookup: 192.0.2.1", "FAIL  login: identity_file: open /nowhere")
	if strings.Contains(out, "TCP connection") || strings.Contains(out, "host key") {
		t.Errorf("stages that did not run are shown:\n%s", out)
	}
}

func TestDoctorDroppedBeforeLogin(t *testing.T) {
	e := newDoctorEnv(t)
	dialTraceFunc = func(_ context.Context, _ config.Remote, _ sftp.Prompter, tr *sftp.Trace) (transport.Transport, error) {
		tr.Addrs = []string{"192.0.2.1"}
		tr.Remote = "192.0.2.1:22"
		tr.HostKey = newTestHostKey(t)
		tr.Failed = sftp.StageAuthStart
		return nil, errors.New("ssh me@nas.test:22: ssh: handshake failed: EOF")
	}
	_, out := e.doctor()
	wantLines(t, out, "OK    host key: ", "FAIL  login: the NAS closed the connection before any login was tried", "may be blocking this computer")
	if strings.Contains(out, "check user") {
		t.Errorf("no login was tried, so its fix does not apply:\n%s", out)
	}
}

func TestDoctorInterrupted(t *testing.T) {
	e := newDoctorEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dialTraceFunc = func(ctx context.Context, _ config.Remote, _ sftp.Prompter, tr *sftp.Trace) (transport.Transport, error) {
		cancel() // Ctrl-C while connecting
		tr.Addrs = []string{"192.0.2.1"}
		tr.Failed = sftp.StageConnect
		return nil, ctx.Err()
	}
	var out bytes.Buffer
	code := executeWith(ctx, []string{"--config", e.cfg, "doctor"}, e.stdin, &out, &out)
	if code != ExitCanceled {
		t.Errorf("exit %d, want %d", code, ExitCanceled)
	}
	for _, s := range []string{"FAIL", "WARN", "SKIP", "No problems"} {
		if strings.Contains(out.String(), s) {
			t.Errorf("reported %s after Ctrl-C:\n%s", s, out.String())
		}
	}
}

package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TatsuyaOGth/camback/internal/applog"
	"github.com/TatsuyaOGth/camback/internal/config"
	"github.com/TatsuyaOGth/camback/internal/meta"
	"github.com/TatsuyaOGth/camback/internal/testutil"
	"github.com/TatsuyaOGth/camback/internal/transport"
	"github.com/TatsuyaOGth/camback/internal/transport/fake"
	"github.com/TatsuyaOGth/camback/internal/transport/sftp"
)

const testConfig = `
[remote.nas]
host = "nas.test"
user = "me"

[[camera]]
id = "GR3x"
match = [{ make = "RICOH IMAGING COMPANY, LTD.", model = "RICOH GR IIIx" }]
remote = "nas"
dest = "/photo/GR3x"

[[camera]]
id = "XE4"
match = [{ make = "FUJIFILM", model = "X-E4" }]
remote = "nas"
dest = "/photo/X-E4"
`

type env struct {
	t     *testing.T
	cfg   string
	card  string
	nas   *fake.FS
	stdin *os.File
}

func newEnv(t *testing.T) *env {
	t.Helper()
	if meta.Check() != nil {
		t.Skip("exiftool not installed")
	}
	home := t.TempDir() // keep logs out of the real home directory
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("LocalAppData", home)
	t.Setenv("XDG_STATE_HOME", home)

	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	os.WriteFile(cfg, []byte(testConfig), 0o644)
	card := filepath.Join(dir, "card")
	testutil.WriteJPEG(t, filepath.Join(card, "DCIM", "100RICOH", "R0001.JPG"), "RICOH IMAGING COMPANY, LTD.", "RICOH GR IIIx")
	os.WriteFile(filepath.Join(card, "DCIM", "100RICOH", "R0001.DNG"), []byte("raw without exif"), 0o644)

	nas := fake.New()
	nas.HashSupported = true
	old := dialFunc
	dialFunc = func(context.Context, config.Remote, sftp.Prompter) (transport.Transport, error) { return nas, nil }
	t.Cleanup(func() { dialFunc = old })

	// stdin that is not a terminal: any prompt fails instead of blocking.
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stdin.Close() })
	return &env{t: t, cfg: cfg, card: card, nas: nas, stdin: stdin}
}

func (e *env) run(args ...string) (int, string) {
	var out bytes.Buffer
	args = append([]string{"--config", e.cfg, "--source", e.card}, args...)
	code := executeWith(context.Background(), args, e.stdin, &out, &out)
	return code, out.String()
}

func TestRunEndToEnd(t *testing.T) {
	e := newEnv(t)

	code, out := e.run("--dry-run")
	if code != ExitOK || !strings.Contains(out, "Camera : GR3x") || !strings.Contains(out, "Dry run") {
		t.Fatalf("dry-run: code %d\n%s", code, out)
	}
	if len(e.nas.Paths()) != 0 {
		t.Fatalf("dry-run wrote files: %v", e.nas.Paths())
	}

	// Without --yes, a non-interactive stdin cannot confirm.
	if code, out := e.run(); code != ExitFailure || !strings.Contains(out, "--yes") {
		t.Fatalf("no --yes: code %d\n%s", code, out)
	}

	code, out = e.run("--yes")
	if code != ExitOK || !strings.Contains(out, "Done: 2 files") {
		t.Fatalf("run: code %d\n%s", code, out)
	}
	if _, ok := e.nas.Get("/photo/GR3x/DCIM/100RICOH/R0001.DNG"); !ok {
		t.Errorf("DNG not transferred: %v", e.nas.Paths())
	}
	logDir, err := applog.Dir() // per-OS location, inside the test's temporary home
	if err != nil {
		t.Fatal(err)
	}
	if logs, _ := filepath.Glob(filepath.Join(logDir, "*.jsonl")); len(logs) != 1 {
		t.Errorf("expected one log file in %s, got %v", logDir, logs)
	}

	code, out = e.run("--yes")
	if code != ExitOK || !strings.Contains(out, "Nothing to transfer") {
		t.Fatalf("rerun: code %d\n%s", code, out)
	}
}

func TestRunConflict(t *testing.T) {
	e := newEnv(t)
	e.nas.Put("/photo/GR3x/DCIM/100RICOH/R0001.DNG", []byte("a different photo"), time.Now())
	code, out := e.run("--yes")
	t.Log("\n" + out) // visible with go test -v
	if code != ExitConflict || !strings.Contains(out, "CONFLICT") || !strings.Contains(out, "nothing was transferred") {
		t.Fatalf("code %d\n%s", code, out)
	}
	if len(e.nas.Paths()) != 1 {
		t.Errorf("nothing else should be written: %v", e.nas.Paths())
	}
}

func TestRunMixedCameras(t *testing.T) {
	e := newEnv(t)
	testutil.WriteJPEG(t, filepath.Join(e.card, "DCIM", "100_FUJI", "DSCF0001.JPG"), "FUJIFILM", "X-E4")
	code, out := e.run("--yes")
	if code != ExitFailure || !strings.Contains(out, "more than one camera") {
		t.Fatalf("code %d\n%s", code, out)
	}
}

func TestInspect(t *testing.T) {
	e := newEnv(t)
	testutil.WriteJPEG(t, filepath.Join(e.card, "DCIM", "100", "X.JPG"), "SONY", "DSC-RX100M7")
	var out bytes.Buffer
	code := executeWith(context.Background(), []string{"--config", e.cfg, "inspect", e.card}, e.stdin, &out, &out)
	s := out.String()
	if code != ExitOK {
		t.Fatalf("code %d\n%s", code, s)
	}
	for _, want := range []string{"camera: GR3x", "no EXIF Make/Model", `{ make = "SONY", model = "DSC-RX100M7" },`} {
		if !strings.Contains(s, want) {
			t.Errorf("output lacks %q:\n%s", want, s)
		}
	}
}

func TestInspectEmptyCard(t *testing.T) {
	e := newEnv(t)
	empty := t.TempDir()
	os.MkdirAll(filepath.Join(empty, "PRIVATE", "M4ROOT"), 0o755)
	os.WriteFile(filepath.Join(empty, "PRIVATE", "M4ROOT", "MEDIAPRO.XML"), []byte("<x/>"), 0o644)
	var out bytes.Buffer
	code := executeWith(context.Background(), []string{"--config", e.cfg, "inspect", empty}, e.stdin, &out, &out)
	s := out.String()
	if code != ExitOK || !strings.Contains(s, "1 other file") || !strings.Contains(s, "no DCIM folder") {
		t.Fatalf("code %d\n%s", code, s)
	}
}

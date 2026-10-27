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
	"github.com/TatsuyaOGth/camback/internal/plan"
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

func TestRunJobs(t *testing.T) {
	e := newEnv(t)
	for _, bad := range []string{"9", "-1", "0"} {
		if code, out := e.run("--yes", "--jobs", bad); code != ExitFailure || !strings.Contains(out, "--jobs must be between 1 and 8") {
			t.Errorf("--jobs %s: code %d\n%s", bad, code, out)
		}
	}
	if len(e.nas.Paths()) != 0 {
		t.Fatalf("rejected runs wrote files: %v", e.nas.Paths())
	}
	if code, out := e.run("--yes", "-j", "1"); code != ExitOK || !strings.Contains(out, "Done: 2 files") {
		t.Fatalf("-j 1: code %d\n%s", code, out)
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

func TestRunOnConflict(t *testing.T) {
	e := newEnv(t)
	e.nas.Put("/photo/GR3x/DCIM/100RICOH/R0001.DNG", []byte("a different photo"), time.Now().Add(-time.Hour))

	if code, out := e.run("--yes", "--on-conflict", "overwrite"); code != ExitFailure || !strings.Contains(out, "--on-conflict must be") {
		t.Fatalf("bad value: code %d\n%s", code, out)
	}
	code, out := e.run("--yes")
	if code != ExitConflict || !strings.Contains(out, "--on-conflict skip or rename") {
		t.Fatalf("abort: code %d\n%s", code, out)
	}

	code, out = e.run("--yes", "--on-conflict", "skip")
	t.Log("\n" + out)
	if code != ExitOK || !strings.Contains(out, "SKIP (conflicting files") || !strings.Contains(out, "Warning: 1 file skipped") {
		t.Fatalf("skip: code %d\n%s", code, out)
	}
	if _, ok := e.nas.Get("/photo/GR3x/DCIM/100RICOH/R0001.JPG"); !ok {
		t.Errorf("JPG not transferred: %v", e.nas.Paths())
	}
	if data, _ := e.nas.Get("/photo/GR3x/DCIM/100RICOH/R0001.DNG"); string(data) != "a different photo" {
		t.Errorf("skipped file was changed: %q", data)
	}

	// Nothing left but the skipped file: no question to ask, and it is still logged.
	code, out = e.run("--on-conflict", "skip")
	if code != ExitOK || !strings.Contains(out, "Nothing to transfer.\nLog: ") || !strings.Contains(out, "Warning: 1 file skipped") {
		t.Fatalf("skip only: code %d\n%s", code, out)
	}
	logDir, err := applog.Dir()
	if err != nil {
		t.Fatal(err)
	}
	logs, _ := filepath.Glob(filepath.Join(logDir, "*.jsonl"))
	if len(logs) != 2 {
		t.Fatalf("expected a log per run with a skipped file, got %v", logs)
	}
	var found bool
	for _, l := range logs {
		data, _ := os.ReadFile(l)
		found = found || strings.Count(string(data), `"msg":"skipped"`) == 1 && !strings.Contains(string(data), `"msg":"transferred"`)
	}
	if !found {
		t.Errorf("no log records the skipped file of the run that transferred nothing")
	}

	fi, err := os.Stat(filepath.Join(e.card, "DCIM", "100RICOH", "R0001.DNG"))
	if err != nil {
		t.Fatal(err)
	}
	renamed := transport.Path("/photo/GR3x/DCIM/100RICOH/" + plan.RenamedName("R0001.DNG", fi.ModTime()))
	code, out = e.run("--yes", "--on-conflict", "rename")
	if code != ExitOK || !strings.Contains(out, "Done: 1 file") {
		t.Fatalf("rename: code %d\n%s", code, out)
	}
	if data, _ := e.nas.Get(renamed); string(data) != "raw without exif" {
		t.Errorf("%s = %q; files: %v", renamed, data, e.nas.Paths())
	}
	if data, _ := e.nas.Get("/photo/GR3x/DCIM/100RICOH/R0001.DNG"); string(data) != "a different photo" {
		t.Errorf("existing file was changed: %q", data)
	}

	// Saved under the timestamped name: no longer a conflict, whatever on_conflict says.
	if code, out := e.run("--yes"); code != ExitOK || !strings.Contains(out, "Nothing to transfer: everything") {
		t.Fatalf("rerun: code %d\n%s", code, out)
	}
}

func TestRunOnConflictConfig(t *testing.T) {
	e := newEnv(t)
	os.WriteFile(e.cfg, []byte(testConfig+"\n[defaults]\non_conflict = \"skip\"\n"), 0o644)
	e.nas.Put("/photo/GR3x/DCIM/100RICOH/R0001.DNG", []byte("a different photo"), time.Now().Add(-time.Hour))

	// The flag wins over the config.
	if code, out := e.run("--dry-run", "--on-conflict", "abort"); code != ExitConflict {
		t.Fatalf("flag: code %d\n%s", code, out)
	}
	if code, out := e.run("--dry-run"); code != ExitOK || !strings.Contains(out, "SKIP") {
		t.Fatalf("config: code %d\n%s", code, out)
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

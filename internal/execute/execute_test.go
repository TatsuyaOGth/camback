package execute

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TatsuyaOGth/camback/internal/config"
	"github.com/TatsuyaOGth/camback/internal/media"
	"github.com/TatsuyaOGth/camback/internal/plan"
	"github.com/TatsuyaOGth/camback/internal/source"
	"github.com/TatsuyaOGth/camback/internal/transport/fake"
)

var mtime = time.Date(2026, 10, 16, 9, 26, 27, 0, time.FixedZone("JST", 9*60*60))

func setup(t *testing.T, files map[string]string) []source.File {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(content), 0o644)
		os.Chtimes(p, mtime, mtime)
	}
	out, err := source.Scan(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func mustPlan(t *testing.T, nas *fake.FS, files []source.File) *plan.Plan {
	t.Helper()
	p, err := plan.Build(context.Background(), nas, "/photo/GR3x", files, plan.Options{IsMedia: media.NewSet(nil).IsMedia})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunAndRerun(t *testing.T) {
	files := setup(t, map[string]string{
		"DCIM/100/A.JPG":    "aaaa",
		"DCIM/100/A.DNG":    "raw-data",
		"MISC/MEDIAPRO.XML": "v2",
	})
	nas := fake.New()
	nas.HashSupported = true
	nas.Put("/photo/GR3x/MISC/MEDIAPRO.XML", []byte("v1"), mtime.Add(-time.Hour))

	var warnings []string
	res, err := Run(context.Background(), nas, mustPlan(t, nas, files), Options{
		Verify: config.VerifyHash,
		Warn:   func(s string) { warnings = append(warnings, s) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 3 || res.VerifyMethod != "sha256sum on destination" {
		t.Errorf("result = %+v", res)
	}
	if len(warnings) > 0 {
		t.Errorf("warnings: %v", warnings)
	}
	if got, _ := nas.Get("/photo/GR3x/DCIM/100/A.JPG"); string(got) != "aaaa" {
		t.Errorf("A.JPG = %q", got)
	}
	if got, _ := nas.Get("/photo/GR3x/MISC/MEDIAPRO.XML"); string(got) != "v1" {
		t.Errorf("original MEDIAPRO.XML must be kept, got %q", got)
	}
	if got, _ := nas.Get("/photo/GR3x/MISC/MEDIAPRO_20261016-002627Z.XML"); string(got) != "v2" {
		t.Errorf("renamed MEDIAPRO = %q", got)
	}
	for _, p := range nas.Paths() {
		if strings.Contains(string(p), TempSuffix) || strings.Contains(string(p), "camback-probe") {
			t.Errorf("leftover %s", p)
		}
	}
	fi, _ := nas.Stat("/photo/GR3x/DCIM/100/A.JPG")
	if !fi.ModTime.Equal(mtime) {
		t.Errorf("mtime not preserved: %v", fi.ModTime)
	}

	// Second run: everything is SAME, nothing to do.
	p2 := mustPlan(t, nas, files)
	if n, _ := p2.Count(plan.Same); n != 3 {
		t.Errorf("rerun: %d SAME, want 3", n)
	}
}

func TestVerifyFallbackAndMismatch(t *testing.T) {
	files := setup(t, map[string]string{"DCIM/A.JPG": "aaaa"})

	// No remote hashing: falls back to read-back without a warning.
	nas := fake.New()
	res, err := Run(context.Background(), nas, mustPlan(t, nas, files), Options{Verify: config.VerifyHash})
	if err != nil {
		t.Fatal(err)
	}
	if res.VerifyMethod != "read-back" {
		t.Errorf("method = %s", res.VerifyMethod)
	}

	// Corrupted write is detected and never moved into place.
	bad := fake.New()
	bad.CorruptWrites = true
	_, err = Run(context.Background(), bad, mustPlan(t, bad, files), Options{Verify: config.VerifyHash})
	if err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("err = %v", err)
	}
	if len(bad.Paths()) != 0 {
		t.Errorf("nothing should remain, got %v", bad.Paths())
	}

	// --verify=size does not hash.
	sz := fake.New()
	sz.HashSupported = true
	res, err = Run(context.Background(), sz, mustPlan(t, sz, files), Options{Verify: config.VerifySize})
	if err != nil {
		t.Fatal(err)
	}
	if res.VerifyMethod != "size" || sz.HashCalls != 0 || sz.OpenCalls != 0 {
		t.Errorf("size mode hashed: %+v hash=%d open=%d", res, sz.HashCalls, sz.OpenCalls)
	}
}

func TestConflictAppearingDuringRun(t *testing.T) {
	files := setup(t, map[string]string{"DCIM/A.JPG": "aaaa"})
	nas := fake.New()
	p := mustPlan(t, nas, files)
	// Someone writes the file after planning.
	nas.Put("/photo/GR3x/DCIM/A.JPG", []byte("other"), mtime)
	_, err := Run(context.Background(), nas, p, Options{Verify: config.VerifySize})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if got, _ := nas.Get("/photo/GR3x/DCIM/A.JPG"); string(got) != "other" {
		t.Errorf("existing file overwritten: %q", got)
	}
}

func TestRefusesConflictPlan(t *testing.T) {
	files := setup(t, map[string]string{"DCIM/A.JPG": "aaaa"})
	nas := fake.New()
	nas.Put("/photo/GR3x/DCIM/A.JPG", []byte("bbbbb"), mtime)
	if _, err := Run(context.Background(), nas, mustPlan(t, nas, files), Options{}); err == nil {
		t.Fatal("expected refusal")
	}
}

func TestCancel(t *testing.T) {
	files := setup(t, map[string]string{"DCIM/A.JPG": "aaaa"})
	nas := fake.New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(ctx, nas, mustPlan(t, nas, files), Options{Verify: config.VerifySize}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

type recProgress struct{ done []bool }

func (r *recProgress) Start(int, int64)              {}
func (r *recProgress) File(int, plan.Item) io.Writer { return io.Discard }
func (r *recProgress) Done(ok bool)                  { r.done = append(r.done, ok) }

func TestProgressDoneReportsOutcome(t *testing.T) {
	files := setup(t, map[string]string{"DCIM/A.JPG": "aaaa"})

	ok := &recProgress{}
	nas := fake.New()
	if _, err := Run(context.Background(), nas, mustPlan(t, nas, files), Options{Verify: config.VerifySize, Progress: ok}); err != nil {
		t.Fatal(err)
	}
	bad := &recProgress{}
	broken := fake.New()
	broken.CorruptWrites = true
	Run(context.Background(), broken, mustPlan(t, broken, files), Options{Verify: config.VerifyHash, Progress: bad})

	if len(ok.done) != 1 || !ok.done[0] {
		t.Errorf("success: Done calls = %v, want [true]", ok.done)
	}
	if len(bad.done) != 1 || bad.done[0] {
		t.Errorf("failure: Done calls = %v, want [false]", bad.done)
	}
}

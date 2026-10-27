package execute

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TatsuyaOGth/camback/internal/config"
	"github.com/TatsuyaOGth/camback/internal/media"
	"github.com/TatsuyaOGth/camback/internal/plan"
	"github.com/TatsuyaOGth/camback/internal/source"
	"github.com/TatsuyaOGth/camback/internal/transport"
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

type recProgress struct {
	mu          sync.Mutex
	started     int
	ended, okay int
	done        []bool
}

func (r *recProgress) Start(int, int64) {}
func (r *recProgress) File(int, plan.Item) io.Writer {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.started++
	return io.Discard
}
func (r *recProgress) FileDone(_ int, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ended++
	if ok {
		r.okay++
	}
}
func (r *recProgress) Done(ok bool) { r.done = append(r.done, ok) }

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

// slowFS delays Create so that transfers overlap, records the most Creates
// in progress at once, and can fail the Create of one temporary file.
type slowFS struct {
	*fake.FS
	delay          time.Duration
	failOn         transport.Path
	inflight, peak atomic.Int32
	mu             sync.Mutex
	created        []transport.Path
}

func (s *slowFS) Create(p transport.Path) (io.WriteCloser, error) {
	n := s.inflight.Add(1)
	defer s.inflight.Add(-1)
	for old := s.peak.Load(); n > old && !s.peak.CompareAndSwap(old, n); old = s.peak.Load() {
	}
	s.mu.Lock()
	s.created = append(s.created, p)
	s.mu.Unlock()
	if p == s.failOn {
		return nil, errors.New("disk full")
	}
	time.Sleep(s.delay)
	return s.FS.Create(p)
}

func manyFiles(t *testing.T, n int) []source.File {
	t.Helper()
	m := map[string]string{}
	for i := range n {
		m[fmt.Sprintf("DCIM/%d/F%02d.JPG", i%3, i)] = fmt.Sprintf("content %d", i)
	}
	return setup(t, m)
}

func TestParallelRespectsJobs(t *testing.T) {
	for _, jobs := range []int{1, 4} {
		t.Run(fmt.Sprint(jobs), func(t *testing.T) {
			files := manyFiles(t, 12)
			nas := &slowFS{FS: fake.New(), delay: 20 * time.Millisecond}
			nas.HashSupported = true
			prog := &recProgress{}
			res, err := Run(context.Background(), nas, mustPlan(t, nas.FS, files), Options{
				Verify: config.VerifyHash, Jobs: jobs, Progress: prog,
			})
			if err != nil {
				t.Fatal(err)
			}
			if res.Files != 12 {
				t.Errorf("result = %+v", res)
			}
			if got := nas.peak.Load(); got != int32(jobs) {
				t.Errorf("peak concurrent transfers = %d, want %d", got, jobs)
			}
			for _, f := range files {
				want, _ := os.ReadFile(f.Abs)
				if got, _ := nas.Get(transport.Path("/photo/GR3x/" + f.Rel)); string(got) != string(want) {
					t.Errorf("%s = %q, want %q", f.Rel, got, want)
				}
			}
			for _, p := range nas.Paths() {
				if strings.Contains(string(p), TempSuffix) || strings.Contains(string(p), "camback-probe") {
					t.Errorf("leftover %s", p)
				}
			}
			if prog.started != 12 || prog.ended != 12 || prog.okay != 12 || len(prog.done) != 1 || !prog.done[0] {
				t.Errorf("progress: started %d, ended %d, ok %d, done %v", prog.started, prog.ended, prog.okay, prog.done)
			}
		})
	}
}

// A failure stops the run: no further file starts, the files in progress
// are cancelled and their temporary files removed, and the error names the
// file that failed (not a cancelled one).
func TestParallelFailureCancelsOthers(t *testing.T) {
	files := manyFiles(t, 20)
	p := mustPlan(t, fake.New(), files)
	items := p.Filter(plan.New)
	bad := items[1]
	nas := &slowFS{FS: fake.New(), delay: 50 * time.Millisecond, failOn: tempPath(bad.Dest)}
	prog := &recProgress{}
	_, err := Run(context.Background(), nas, p, Options{Verify: config.VerifySize, Jobs: 4, Progress: prog})
	if err == nil || !strings.Contains(err.Error(), bad.File.Rel) || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v, want the failure of %s", err, bad.File.Rel)
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the original failure, not a cancellation", err)
	}
	if paths := nas.Paths(); len(paths) != 0 {
		t.Errorf("files left on the destination: %v", paths)
	}
	if len(nas.created) > 4 {
		t.Errorf("%d files were started after the failure: %v", len(nas.created)-4, nas.created[4:])
	}
	if prog.started != prog.ended || prog.okay != 0 || len(prog.done) != 1 || prog.done[0] {
		t.Errorf("progress: started %d, ended %d, ok %d, done %v", prog.started, prog.ended, prog.okay, prog.done)
	}
}

// A card file named like the timestamped name of another, changed file
// would share its destination (and temporary file) with it.
func TestModes(t *testing.T) {
	files := setup(t, map[string]string{
		"DCIM/100/A.JPG": "aaaa",
		"MISC/B.XML":     "b",
	})
	nas := fake.New()
	nas.HashSupported = true
	nas.MkdirAll("/photo") // exists before the run: keeps its mode
	_, err := Run(context.Background(), nas, mustPlan(t, nas, files), Options{
		Verify: config.VerifyHash, FileMode: 0o640, DirMode: 0o750,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[transport.Path]fs.FileMode{
		"/photo":                     0o777,
		"/photo/GR3x":                0o750,
		"/photo/GR3x/DCIM":           0o750,
		"/photo/GR3x/DCIM/100":       0o750,
		"/photo/GR3x/MISC":           0o750,
		"/photo/GR3x/DCIM/100/A.JPG": 0o640,
		"/photo/GR3x/MISC/B.XML":     0o640,
	}
	for p, m := range want {
		if fi, err := nas.Stat(p); err != nil || fi.Mode != m {
			t.Errorf("%s: mode %v, %v; want %v", p, fi.Mode, err, m)
		}
	}
	// Files are changed under their temporary name, never in place.
	for _, p := range nas.ChmodCalls {
		if b := p.Base(); strings.HasPrefix(b, "A.") || strings.HasPrefix(b, "B.") {
			t.Errorf("chmod on final name %s", p)
		}
	}
}

func TestModesNotSetByDefault(t *testing.T) {
	files := setup(t, map[string]string{"DCIM/100/A.JPG": "aaaa"})
	nas := fake.New()
	if _, err := Run(context.Background(), nas, mustPlan(t, nas, files), Options{Verify: config.VerifySize}); err != nil {
		t.Fatal(err)
	}
	if len(nas.ChmodCalls) > 0 {
		t.Errorf("chmod called without modes: %v", nas.ChmodCalls)
	}
}

func TestModesIgnoredByServer(t *testing.T) {
	for _, opt := range []Options{
		{Verify: config.VerifySize, FileMode: 0o644},
		{Verify: config.VerifySize, DirMode: 0o755},
	} {
		t.Run(fmt.Sprintf("file %o dir %o", opt.FileMode, opt.DirMode), func(t *testing.T) {
			files := setup(t, map[string]string{"DCIM/100/A.JPG": "aaaa"})
			nas := fake.New()
			nas.IgnoreChmod = true
			res, err := Run(context.Background(), nas, mustPlan(t, nas, files), opt)
			if err == nil || !strings.Contains(err.Error(), "does not apply permissions") {
				t.Fatalf("err = %v", err)
			}
			if res.Files != 0 {
				t.Errorf("transferred %d files", res.Files)
			}
			for _, p := range nas.Paths() {
				t.Errorf("file left on the destination: %s", p)
			}
		})
	}
}

// aclFS ignores chmod under one directory, like a folder with its own ACL.
type aclFS struct {
	*fake.FS
	under transport.Path
}

func (a *aclFS) Chmod(p transport.Path, mode fs.FileMode) error {
	if strings.HasPrefix(string(p), string(a.under)+"/") {
		return nil
	}
	return a.FS.Chmod(p, mode)
}

// The probe file in the destination passes, but a file in a directory that
// ignores chmod must not get its final name.
func TestFileModeCheckedForEveryFile(t *testing.T) {
	files := setup(t, map[string]string{"DCIM/100/A.JPG": "aaaa"})
	nas := &aclFS{FS: fake.New(), under: "/photo/GR3x/DCIM"}
	nas.MkdirAll("/photo/GR3x/DCIM/100")
	res, err := Run(context.Background(), nas, mustPlan(t, nas.FS, files), Options{Verify: config.VerifySize, FileMode: 0o644})
	if err == nil || !strings.Contains(err.Error(), "does not apply permissions") {
		t.Fatalf("err = %v", err)
	}
	if res.Files != 0 {
		t.Errorf("transferred %d files", res.Files)
	}
	for _, p := range nas.Paths() {
		t.Errorf("file left on the destination: %s", p)
	}
}

// racyFS creates a directory itself just before camback does, like another
// process would.
type racyFS struct {
	*fake.FS
	race transport.Path
}

func (r *racyFS) Mkdir(p transport.Path) error {
	if p == r.race {
		r.FS.Mkdir(p)
	}
	return r.FS.Mkdir(p)
}

func TestDirModeLeavesDirectoryCreatedByOthers(t *testing.T) {
	files := setup(t, map[string]string{"DCIM/100/A.JPG": "aaaa"})
	nas := &racyFS{FS: fake.New(), race: "/photo/GR3x/DCIM"}
	if _, err := Run(context.Background(), nas, mustPlan(t, nas.FS, files), Options{Verify: config.VerifySize, DirMode: 0o750}); err != nil {
		t.Fatal(err)
	}
	want := map[transport.Path]fs.FileMode{
		"/photo/GR3x":          0o750,
		"/photo/GR3x/DCIM":     0o777, // not created by camback
		"/photo/GR3x/DCIM/100": 0o750,
	}
	for p, m := range want {
		if fi, err := nas.Stat(p); err != nil || fi.Mode != m {
			t.Errorf("%s: mode %v, %v; want %v", p, fi.Mode, err, m)
		}
	}
}

func TestDuplicateDestinationRefused(t *testing.T) {
	files := setup(t, map[string]string{
		"MISC/STATE.XML":                  "new state",
		"MISC/STATE_20261016-002627Z.XML": "copied back to the card",
	})
	nas := fake.New()
	nas.Put("/photo/GR3x/MISC/STATE.XML", []byte("old"), mtime.Add(-time.Hour))
	p := mustPlan(t, nas, files)
	if n, _ := p.Count(plan.Rename); n != 1 {
		t.Fatalf("plan: %d RENAME, want 1", n)
	}
	_, err := Run(context.Background(), nas, p, Options{Verify: config.VerifySize, Jobs: 4})
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "would both be saved as") {
		t.Fatalf("err = %v, want a duplicate destination conflict", err)
	}
	if paths := nas.Paths(); len(paths) != 1 {
		t.Errorf("nothing should be written: %v", paths)
	}
}

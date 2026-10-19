package plan

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/TatsuyaOGth/camback/internal/media"
	"github.com/TatsuyaOGth/camback/internal/source"
	"github.com/TatsuyaOGth/camback/internal/transport"
	"github.com/TatsuyaOGth/camback/internal/transport/fake"
)

var base = time.Date(2026, 10, 16, 9, 26, 27, 0, time.Local)

// card writes files to a temp "SD card" and returns them as source.Files.
func card(t *testing.T, files map[string]string, mtime time.Time) []source.File {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, mtime, mtime)
	}
	out, err := source.Scan(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func build(t *testing.T, nas *fake.FS, files []source.File) *Plan {
	t.Helper()
	p, err := Build(context.Background(), nas, "/photo/GR3x", files, Options{IsMedia: media.NewSet(nil).IsMedia})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func actions(p *Plan) map[string]Action {
	m := map[string]Action{}
	for _, it := range p.Items {
		m[it.File.Rel] = it.Action
	}
	return m
}

func TestBuild(t *testing.T) {
	files := card(t, map[string]string{
		"DCIM/100/NEW.JPG":   "new",
		"DCIM/100/SAME.JPG":  "same",
		"DCIM/100/SHIFT.JPG": "shift", // same content, mtime off by 9h (timezone)
		"DCIM/100/DIFF.DNG":  "AAAA",  // same size, different content, mtime off
		"DCIM/100/SIZE.JPG":  "longer content",
		"DCIM/100/NEAR.JPG":  "near", // mtime within 2s
		"MISC/MEDIAPRO.XML":  "v2",
		"MISC/NEW.XML":       "x",
		"MISC/SAVED.XML":     "v3",
		"MISC/CLASH.XML":     "v4",
	}, base)

	nas := fake.New()
	nas.Put("/photo/GR3x/DCIM/100/SAME.JPG", []byte("same"), base)
	nas.Put("/photo/GR3x/DCIM/100/SHIFT.JPG", []byte("shift"), base.Add(9*time.Hour))
	nas.Put("/photo/GR3x/DCIM/100/DIFF.DNG", []byte("BBBB"), base.Add(time.Hour))
	nas.Put("/photo/GR3x/DCIM/100/SIZE.JPG", []byte("short"), base)
	nas.Put("/photo/GR3x/DCIM/100/NEAR.JPG", []byte("near"), base.Add(2*time.Second))
	nas.Put("/photo/GR3x/MISC/MEDIAPRO.XML", []byte("v1"), base.Add(-time.Hour))
	// Earlier run already saved this version under the renamed name.
	nas.Put("/photo/GR3x/MISC/SAVED.XML", []byte("old"), base.Add(-time.Hour))
	nas.Put("/photo/GR3x/MISC/SAVED_20261016-092627.XML", []byte("v3"), base)
	// Renamed target exists with different content.
	nas.Put("/photo/GR3x/MISC/CLASH.XML", []byte("old"), base.Add(-time.Hour))
	nas.Put("/photo/GR3x/MISC/CLASH_20261016-092627.XML", []byte("zz"), base.Add(time.Hour))

	p := build(t, nas, files)
	want := map[string]Action{
		"DCIM/100/NEW.JPG":   New,
		"DCIM/100/SAME.JPG":  Same,
		"DCIM/100/SHIFT.JPG": Same,
		"DCIM/100/DIFF.DNG":  Conflict,
		"DCIM/100/SIZE.JPG":  Conflict,
		"DCIM/100/NEAR.JPG":  Same,
		"MISC/MEDIAPRO.XML":  Rename,
		"MISC/NEW.XML":       New,
		"MISC/SAVED.XML":     Same,
		"MISC/CLASH.XML":     Conflict,
	}
	got := actions(p)
	for rel, a := range want {
		if got[rel] != a {
			t.Errorf("%s: got %s, want %s", rel, got[rel], a)
		}
	}
	for _, it := range p.Items {
		if it.File.Rel == "MISC/MEDIAPRO.XML" && it.Dest != "/photo/GR3x/MISC/MEDIAPRO_20261016-092627.XML" {
			t.Errorf("rename dest = %s", it.Dest)
		}
	}
	if n, bytes := p.Count(New); n != 2 || bytes != 4 {
		t.Errorf("New count = %d, %d bytes", n, bytes)
	}
	// Stage 3 (read-back hash) only runs when size matches but mtime does
	// not: SHIFT, DIFF, MEDIAPRO, and CLASH's renamed target.
	if nas.OpenCalls != 4 {
		t.Errorf("read-backs = %d, want 4", nas.OpenCalls)
	}
}

func TestBuildDirectoryAtTarget(t *testing.T) {
	files := card(t, map[string]string{"DCIM/100/A.JPG": "a"}, base)
	nas := fake.New()
	nas.MkdirAll(transport.Path("/photo/GR3x/DCIM/100/A.JPG"))
	if got := actions(build(t, nas, files))["DCIM/100/A.JPG"]; got != Conflict {
		t.Errorf("got %s", got)
	}
}

func TestRenamedName(t *testing.T) {
	cases := map[string]string{
		"MEDIAPRO.XML": "MEDIAPRO_20261016-092627.XML",
		"a.tar.gz":     "a.tar_20261016-092627.gz",
		"README":       "README_20261016-092627",
	}
	for in, want := range cases {
		if got := RenamedName(in, base); got != want {
			t.Errorf("RenamedName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMtimeClose(t *testing.T) {
	if !MtimeClose(base, base.Add(2*time.Second+900*time.Millisecond)) {
		t.Error("2.9s (2s after truncation) should be close")
	}
	if MtimeClose(base, base.Add(3*time.Second)) {
		t.Error("3s should not be close")
	}
	if !MtimeClose(base.In(time.UTC), base) {
		t.Error("same instant in different locations must be equal")
	}
}

func TestBuildCancelled(t *testing.T) {
	files := card(t, map[string]string{"DCIM/100/A.JPG": "a"}, base)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Build(ctx, fake.New(), "/photo/GR3x", files, Options{IsMedia: media.NewSet(nil).IsMedia}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

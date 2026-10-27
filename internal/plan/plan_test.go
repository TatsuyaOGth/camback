package plan

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/TatsuyaOGth/camback/internal/config"
	"github.com/TatsuyaOGth/camback/internal/media"
	"github.com/TatsuyaOGth/camback/internal/source"
	"github.com/TatsuyaOGth/camback/internal/transport"
	"github.com/TatsuyaOGth/camback/internal/transport/fake"
)

var base = time.Date(2026, 10, 16, 9, 26, 27, 0, time.FixedZone("JST", 9*60*60))

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
	return buildWith(t, nas, files, "")
}

func buildWith(t *testing.T, nas *fake.FS, files []source.File, onConflict string) *Plan {
	t.Helper()
	p, err := Build(context.Background(), nas, "/photo/GR3x", files, Options{IsMedia: media.NewSet(nil).IsMedia, OnConflict: onConflict})
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
		"MISC/OLDNAME.XML":   "v5",
		"MISC/OLDDIFF.XML":   "v6",
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
	nas.Put("/photo/GR3x/MISC/SAVED_20261016-002627Z.XML", []byte("v3"), base)
	// Renamed target exists with different content.
	nas.Put("/photo/GR3x/MISC/CLASH.XML", []byte("old"), base.Add(-time.Hour))
	nas.Put("/photo/GR3x/MISC/CLASH_20261016-002627Z.XML", []byte("zz"), base.Add(time.Hour))
	// An older version saved this one under its local-time name.
	nas.Put("/photo/GR3x/MISC/OLDNAME.XML", []byte("old"), base.Add(-time.Hour))
	nas.Put(transport.Path("/photo/GR3x/MISC/"+localRenamedName("OLDNAME.XML", base)), []byte("v5"), base)
	// A local-time name with other content is no reason to stop.
	nas.Put("/photo/GR3x/MISC/OLDDIFF.XML", []byte("old"), base.Add(-time.Hour))
	nas.Put(transport.Path("/photo/GR3x/MISC/"+localRenamedName("OLDDIFF.XML", base)), []byte("zz"), base.Add(time.Hour))

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
		"MISC/OLDNAME.XML":   Same,
		"MISC/OLDDIFF.XML":   Rename,
	}
	got := actions(p)
	for rel, a := range want {
		if got[rel] != a {
			t.Errorf("%s: got %s, want %s", rel, got[rel], a)
		}
	}
	wantDest := map[string]transport.Path{
		"MISC/MEDIAPRO.XML": "/photo/GR3x/MISC/MEDIAPRO_20261016-002627Z.XML",
		"MISC/OLDNAME.XML":  transport.Path("/photo/GR3x/MISC/" + localRenamedName("OLDNAME.XML", base)),
		"MISC/OLDDIFF.XML":  "/photo/GR3x/MISC/OLDDIFF_20261016-002627Z.XML",
	}
	for _, it := range p.Items {
		if d, ok := wantDest[it.File.Rel]; ok && it.Dest != d {
			t.Errorf("%s: dest = %s, want %s", it.File.Rel, it.Dest, d)
		}
	}
	if n, bytes := p.Count(New); n != 2 || bytes != 4 {
		t.Errorf("New count = %d, %d bytes", n, bytes)
	}
	// Stage 3 (read-back hash) only runs when size matches but mtime does
	// not: SHIFT, DIFF, MEDIAPRO, CLASH's renamed target, and OLDDIFF's
	// local-time name.
	if nas.OpenCalls != 5 {
		t.Errorf("read-backs = %d, want 5", nas.OpenCalls)
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
		"MEDIAPRO.XML": "MEDIAPRO_20261016-002627Z.XML",
		"a.tar.gz":     "a.tar_20261016-002627Z.gz",
		"README":       "README_20261016-002627Z",
	}
	for in, want := range cases {
		if got := RenamedName(in, base); got != want {
			t.Errorf("RenamedName(%q) = %q, want %q", in, got, want)
		}
	}
}

// The name must not depend on the time zone of the PC (issue #2).
func TestRenamedNameIgnoresLocalTimeZone(t *testing.T) {
	saved := time.Local
	t.Cleanup(func() { time.Local = saved })
	var names []string
	for _, tz := range []*time.Location{time.FixedZone("JST", 9*60*60), time.UTC, time.FixedZone("EST", -5*60*60)} {
		time.Local = tz
		names = append(names, RenamedName("MEDIAPRO.XML", base))
	}
	for _, n := range names[1:] {
		if n != names[0] {
			t.Errorf("names differ by time zone: %q", names)
			break
		}
	}
}

func TestLocalRenamedName(t *testing.T) {
	saved := time.Local
	t.Cleanup(func() { time.Local = saved })
	time.Local = time.FixedZone("JST", 9*60*60)
	if got := localRenamedName("MEDIAPRO.XML", base.UTC()); got != "MEDIAPRO_20261016-092627.XML" {
		t.Errorf("localRenamedName = %q", got)
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

func TestBuildOnConflict(t *testing.T) {
	files := card(t, map[string]string{
		"DCIM/100/DIFF.DNG":  "AAAA",
		"DCIM/100/SAVED.DNG": "v2",
		"DCIM/100/DIR.JPG":   "d",
		"MISC/CLASH.XML":     "v4",
	}, base)
	nas := fake.New()
	nas.Put("/photo/GR3x/DCIM/100/DIFF.DNG", []byte("BBBB"), base.Add(time.Hour))
	// An earlier run with on_conflict = rename saved this one.
	nas.Put("/photo/GR3x/DCIM/100/SAVED.DNG", []byte("v1"), base.Add(-time.Hour))
	nas.Put("/photo/GR3x/DCIM/100/SAVED_20261016-002627Z.DNG", []byte("v2"), base)
	nas.MkdirAll("/photo/GR3x/DCIM/100/DIR.JPG")
	nas.Put("/photo/GR3x/MISC/CLASH.XML", []byte("old"), base.Add(-time.Hour))
	nas.Put("/photo/GR3x/MISC/CLASH_20261016-002627Z.XML", []byte("zz"), base.Add(time.Hour))

	cases := []struct {
		onConflict string
		want       map[string]Action
	}{
		{"", map[string]Action{"DCIM/100/DIFF.DNG": Conflict, "DCIM/100/SAVED.DNG": Same, "DCIM/100/DIR.JPG": Conflict, "MISC/CLASH.XML": Conflict}},
		{config.OnConflictAbort, map[string]Action{"DCIM/100/DIFF.DNG": Conflict, "DCIM/100/SAVED.DNG": Same, "DCIM/100/DIR.JPG": Conflict, "MISC/CLASH.XML": Conflict}},
		{config.OnConflictSkip, map[string]Action{"DCIM/100/DIFF.DNG": Skip, "DCIM/100/SAVED.DNG": Same, "DCIM/100/DIR.JPG": Skip, "MISC/CLASH.XML": Skip}},
		// The renamed target of CLASH.XML holds yet another file: nothing to rename to.
		{config.OnConflictRename, map[string]Action{"DCIM/100/DIFF.DNG": Rename, "DCIM/100/SAVED.DNG": Same, "DCIM/100/DIR.JPG": Rename, "MISC/CLASH.XML": Conflict}},
	}
	for _, c := range cases {
		p := buildWith(t, nas, files, c.onConflict)
		got := actions(p)
		for rel, a := range c.want {
			if got[rel] != a {
				t.Errorf("on_conflict %q: %s: got %s, want %s", c.onConflict, rel, got[rel], a)
			}
		}
		for _, it := range p.Items {
			if it.File.Rel == "DCIM/100/DIFF.DNG" && it.Action == Rename && it.Dest != "/photo/GR3x/DCIM/100/DIFF_20261016-002627Z.DNG" {
				t.Errorf("DIFF.DNG renamed to %s", it.Dest)
			}
		}
	}
}

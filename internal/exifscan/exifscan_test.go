package exifscan

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/TatsuyaOGth/camback/internal/meta"
	"github.com/TatsuyaOGth/camback/internal/source"
)

var gr = meta.Info{Make: "RICOH IMAGING COMPANY, LTD.", Model: "RICOH GR IIIx"}

// fakeReader stands in for exiftool: it knows the EXIF of some files and
// records which files it was asked to read.
type fakeReader struct {
	exif  map[string]meta.Info // files exiftool can read
	calls [][]string
}

func (r *fakeReader) read(_ context.Context, _ string, rels []string) (map[string]meta.Info, error) {
	r.calls = append(r.calls, append([]string(nil), rels...))
	out := map[string]meta.Info{}
	for _, rel := range rels {
		if info, ok := r.exif[rel]; ok {
			out[rel] = info
		}
	}
	return out, nil
}

func (r *fakeReader) last() []string {
	if len(r.calls) == 0 {
		return nil
	}
	return r.calls[len(r.calls)-1]
}

var t0 = time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)

func file(rel string, size int64) source.File {
	return source.File{Rel: rel, Size: size, ModTime: t0}
}

func TestCache(t *testing.T) {
	dir := t.TempDir()
	r := &fakeReader{exif: map[string]meta.Info{
		"DCIM/100/R1.JPG": gr,
		"DCIM/100/R1.DNG": gr,
		"DCIM/100/R2.MOV": {}, // no EXIF Make/Model
	}}
	var warnings []string
	opt := Options{CacheDir: dir, Volume: "RICOH GR", Read: r.read, Warn: func(s string) { warnings = append(warnings, s) }}
	files := []source.File{file("DCIM/100/R1.JPG", 10), file("DCIM/100/R1.DNG", 20), file("DCIM/100/R2.MOV", 30)}

	res, err := Scan(context.Background(), "/card", files, opt)
	if err != nil {
		t.Fatal(err)
	}
	if res.Cached != 0 || len(res.Infos) != 3 || len(r.last()) != 3 {
		t.Fatalf("first run: cached %d, infos %v, read %v", res.Cached, res.Infos, r.last())
	}

	// Second run: only the file without EXIF is read again.
	res, err = Scan(context.Background(), "/card", files, opt)
	if err != nil {
		t.Fatal(err)
	}
	if res.Cached != 2 || !reflect.DeepEqual(r.last(), []string{"DCIM/100/R2.MOV"}) {
		t.Fatalf("second run: cached %d, read %v", res.Cached, r.last())
	}
	if res.Infos["DCIM/100/R1.DNG"] != gr {
		t.Errorf("cached info = %+v", res.Infos["DCIM/100/R1.DNG"])
	}

	// A file whose size or mtime changed is read again; a new file is read.
	files[1].Size = 21
	files[0].ModTime = t0.Add(time.Second)
	files = append(files, file("DCIM/100/R3.JPG", 40))
	r.exif["DCIM/100/R3.JPG"] = gr
	res, err = Scan(context.Background(), "/card", files, opt)
	if err != nil {
		t.Fatal(err)
	}
	got := r.last()
	sort.Strings(got)
	if want := []string{"DCIM/100/R1.DNG", "DCIM/100/R1.JPG", "DCIM/100/R2.MOV", "DCIM/100/R3.JPG"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("changed files: read %v, want %v", got, want)
	}

	// Files gone from the card are dropped from the cache.
	if _, err := Scan(context.Background(), "/card", files[3:], opt); err != nil {
		t.Fatal(err)
	}
	c, err := load(filepath.Join(dir, fileName("RICOH GR")))
	if err != nil {
		t.Fatal(err)
	}
	if len(c) != 1 || c["DCIM/100/R3.JPG"].Model != gr.Model {
		t.Errorf("cache after removal = %+v", c)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
}

func TestCacheOff(t *testing.T) {
	r := &fakeReader{exif: map[string]meta.Info{"DCIM/100/R1.JPG": gr}}
	files := []source.File{file("DCIM/100/R1.JPG", 10)}
	for i := 0; i < 2; i++ {
		res, err := Scan(context.Background(), "/card", files, Options{Read: r.read})
		if err != nil {
			t.Fatal(err)
		}
		if res.Cached != 0 || len(r.last()) != 1 {
			t.Fatalf("run %d: cached %d, read %v", i, res.Cached, r.last())
		}
	}
}

func TestCacheCorrupt(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, fileName("CARD"))
	os.WriteFile(p, []byte("{not json"), 0o600)
	r := &fakeReader{exif: map[string]meta.Info{"A.JPG": gr}}
	var warnings []string
	opt := Options{CacheDir: dir, Volume: "CARD", Read: r.read, Warn: func(s string) { warnings = append(warnings, s) }}
	if _, err := Scan(context.Background(), "/card", []source.File{file("A.JPG", 1)}, opt); err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "rebuilt") {
		t.Errorf("warnings = %v", warnings)
	}
	if c, err := load(p); err != nil || len(c) != 1 {
		t.Errorf("cache was not rebuilt: %v, %v", c, err)
	}
}

func TestCacheUnwritable(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	os.WriteFile(blocker, nil, 0o600)
	r := &fakeReader{exif: map[string]meta.Info{"A.JPG": gr}}
	var warnings []string
	// The cache "directory" is a file: saving fails, but the scan does not.
	opt := Options{CacheDir: blocker, Volume: "CARD", Read: r.read, Warn: func(s string) { warnings = append(warnings, s) }}
	res, err := Scan(context.Background(), "/card", []source.File{file("A.JPG", 1)}, opt)
	if err != nil || len(res.Infos) != 1 {
		t.Fatalf("scan: %v, %+v", err, res)
	}
	if len(warnings) == 0 || !strings.Contains(warnings[len(warnings)-1], "could not save") {
		t.Errorf("warnings = %v", warnings)
	}
}

func TestSample(t *testing.T) {
	rels := []string{
		"DCIM/100/R3.JPG", "DCIM/100/R1.JPG", "DCIM/100/R2.JPG",
		"DCIM/100/R1.DNG", "DCIM/100/R2.dng",
		"DCIM/100/R9.MOV",
		"DCIM/101/A1.JPG", "DCIM/101/A2.JPG",
	}
	want := []string{
		"DCIM/100/R1.DNG", "DCIM/100/R1.JPG", "DCIM/100/R2.dng", "DCIM/100/R3.JPG", "DCIM/100/R9.MOV",
		"DCIM/101/A1.JPG", "DCIM/101/A2.JPG",
	}
	if got := Sample(rels); !reflect.DeepEqual(got, want) {
		t.Errorf("Sample = %v\nwant     %v", got, want)
	}
	if got := Sample(nil); len(got) != 0 {
		t.Errorf("Sample(nil) = %v", got)
	}
}

func TestScanSample(t *testing.T) {
	dir := t.TempDir()
	r := &fakeReader{exif: map[string]meta.Info{}}
	var files []source.File
	for _, n := range []string{"R1", "R2", "R3", "R4"} {
		rel := "DCIM/100/" + n + ".JPG"
		files = append(files, file(rel, 1))
		r.exif[rel] = gr
	}
	opt := Options{CacheDir: dir, Volume: "CARD", Sample: true, Read: r.read}
	res, err := Scan(context.Background(), "/card", files, opt)
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped != 2 || !reflect.DeepEqual(r.last(), []string{"DCIM/100/R1.JPG", "DCIM/100/R4.JPG"}) {
		t.Fatalf("first: skipped %d, read %v", res.Skipped, r.last())
	}

	// The sampled files are now cached; the sample comes from the others.
	res, err = Scan(context.Background(), "/card", files, opt)
	if err != nil {
		t.Fatal(err)
	}
	if res.Cached != 2 || res.Skipped != 0 || !reflect.DeepEqual(r.last(), []string{"DCIM/100/R2.JPG", "DCIM/100/R3.JPG"}) {
		t.Fatalf("second: cached %d, skipped %d, read %v", res.Cached, res.Skipped, r.last())
	}
}

func TestFileName(t *testing.T) {
	a, b := fileName("RICOH GR"), fileName("RICOH_GR")
	if a == b {
		t.Errorf("labels that differ only in replaced characters share %q", a)
	}
	if strings.ContainsAny(a, ` /\:`) || !strings.HasPrefix(a, "exif-RICOH_GR-") {
		t.Errorf("fileName = %q", a)
	}
	if long := fileName(strings.Repeat("x", 100)); len(long) > 70 {
		t.Errorf("long label gives %q", long)
	}
}

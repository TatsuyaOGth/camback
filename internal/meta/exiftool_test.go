package meta

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/TatsuyaOGth/camback/internal/testutil"
)

func TestRead(t *testing.T) {
	if Check() != nil {
		t.Skip("exiftool not installed")
	}
	dir := t.TempDir()
	testutil.WriteJPEG(t, filepath.Join(dir, "DCIM", "100RICOH", "R0001.JPG"), "RICOH IMAGING COMPANY, LTD.", "RICOH GR IIIx")
	testutil.WriteJPEG(t, filepath.Join(dir, "DCIM", "日本語", "名前 with space.JPG"), "FUJIFILM", "X-E4")
	testutil.WriteJPEG(t, filepath.Join(dir, "DCIM", "noexif.JPG"), "", "")
	testutil.WriteJPEG(t, filepath.Join(dir, "-dash.JPG"), "SONY", "ILCE-6400")
	if err := os.WriteFile(filepath.Join(dir, "broken.JPG"), []byte("not a jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Read(context.Background(), dir, []string{
		"DCIM/100RICOH/R0001.JPG",
		"DCIM/日本語/名前 with space.JPG",
		"DCIM/noexif.JPG",
		"broken.JPG",
		"missing.JPG",
		"-dash.JPG",
	})
	if err != nil {
		t.Fatal(err)
	}
	if i := got["DCIM/100RICOH/R0001.JPG"]; i.Make != "RICOH IMAGING COMPANY, LTD." || i.Model != "RICOH GR IIIx" {
		t.Errorf("GR: %+v", i)
	}
	if i := got["DCIM/日本語/名前 with space.JPG"]; i.Model != "X-E4" {
		t.Errorf("unicode path: %+v (all: %+v)", i, got)
	}
	if i := got["-dash.JPG"]; i.Model != "ILCE-6400" {
		t.Errorf("leading dash: %+v", i)
	}
	if i := got["DCIM/noexif.JPG"]; i.Identified() {
		t.Errorf("noexif should not be identified: %+v", i)
	}
	if i := got["broken.JPG"]; i.Identified() {
		t.Errorf("broken should not be identified: %+v", i)
	}
}

func TestFlexString(t *testing.T) {
	var r record
	if err := json.Unmarshal([]byte(`{"SourceFile":"a","Make":"X","Model":5}`), &r); err != nil {
		t.Fatal(err)
	}
	if r.Model != "5" {
		t.Errorf("Model = %q", r.Model)
	}
}

package source

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, root, rel string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(rel), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScan(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{
		"DCIM/100RICOH/R0001.JPG",
		"DCIM/100RICOH/R0001.DNG",
		"DCIM/100RICOH/._R0001.JPG",
		".Spotlight-V100/store.db",
		"System Volume Information/IndexerVolumeGuid",
		"MISC/info.xml",
		"PRIVATE/tmp/cache.bin",
	} {
		write(t, root, rel)
	}
	files, err := Scan(root, []string{"PRIVATE/tmp/*"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range files {
		got = append(got, f.Rel)
	}
	want := []string{"DCIM/100RICOH/R0001.DNG", "DCIM/100RICOH/R0001.JPG", "MISC/info.xml"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %v want %v", got, want)
		}
	}
	if files[0].Size != int64(len(want[0])) {
		t.Errorf("size = %d", files[0].Size)
	}
}

func TestExcluded(t *testing.T) {
	cases := []struct {
		rel  string
		pats []string
		want bool
	}{
		{"DCIM/100RICOH/R0001.JPG", nil, false},
		{"DCIM/.R0001.JPG.camback-part", nil, true},
		{"$RECYCLE.BIN/x", nil, true},
		{"thumbs.db", nil, true},
		{"DCIM/100RICOH/R0001.THM", []string{"*.THM"}, true},
		{"MISC/a.xml", []string{"MISC/*"}, true},
		{"DCIM/MISC/a.xml", []string{"MISC/*"}, false},
	}
	for _, tc := range cases {
		if got := Excluded(tc.rel, tc.pats); got != tc.want {
			t.Errorf("Excluded(%q, %v) = %v, want %v", tc.rel, tc.pats, got, tc.want)
		}
	}
}

func TestHasDCIM(t *testing.T) {
	root := t.TempDir()
	if HasDCIM(root) {
		t.Error("empty dir has no DCIM")
	}
	write(t, root, "DCIM/100RICOH/R0001.JPG")
	if !HasDCIM(root) {
		t.Error("DCIM not detected")
	}
}

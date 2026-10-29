package camera

import (
	"errors"
	"strings"
	"testing"

	"github.com/TatsuyaOGth/camback/internal/config"
	"github.com/TatsuyaOGth/camback/internal/meta"
)

func testConfig() *config.Config {
	return &config.Config{Cameras: []config.Camera{
		{ID: "GR3", Match: []config.Match{{Make: "RICOH", Model: "GR III"}}},
		{ID: "GR3x", Match: []config.Match{{Make: "RICOH", Model: "GR IIIx"}}},
		{ID: "Go3", VolumeLabel: "Insta360GO3"},
		{ID: "XE4", Match: []config.Match{{Make: "FUJIFILM", Model: "X-E4"}}, VolumeLabel: "Untitled"},
	}}
}

var (
	gr3  = meta.Info{Make: "RICOH", Model: "GR III"}
	gr3x = meta.Info{Make: "RICOH", Model: "GR IIIx"}
	none = meta.Info{}
)

func TestResolve(t *testing.T) {
	cfg := testConfig()
	cases := []struct {
		name    string
		infos   map[string]meta.Info
		label   string
		flag    string
		wantID  string
		wantBy  Method
		wantErr string
	}{
		{"unanimous", map[string]meta.Info{"a.JPG": gr3x, "a.DNG": gr3x, "b.MP4": none}, "RICOH GR", "", "GR3x", ByEXIF, ""},
		{"flag agrees", map[string]meta.Info{"a.JPG": gr3x}, "", "GR3x", "GR3x", ByEXIF, ""},
		{"flag contradicts", map[string]meta.Info{"a.JPG": gr3x}, "", "GR3", "", "", "contradicts"},
		{"mixed", map[string]meta.Info{"a.JPG": gr3x, "b.JPG": gr3}, "", "", "", "", "more than one camera"},
		{"unknown", map[string]meta.Info{"a.JPG": gr3x, "c.JPG": {Make: "SONY", Model: "DSC-RX100M7"}}, "", "", "", "", `model = "DSC-RX100M7"`},
		{"label fallback", map[string]meta.Info{"a.MP4": none}, "insta360go3", "", "Go3", ByVolumeLabel, ""},
		{"flag when no exif", map[string]meta.Info{}, "Untitled", "Go3", "Go3", ByFlag, ""},
		{"unknown flag", map[string]meta.Info{}, "", "nope", "", "", "no such camera"},
		{"undetermined", map[string]meta.Info{"a.MP4": none}, "SOMETHING", "", "", "", "cannot determine"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Resolve(cfg, tc.infos, tc.label, tc.flag)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if res.Camera.ID != tc.wantID || res.Method != tc.wantBy {
				t.Errorf("got %s by %s, want %s by %s", res.Camera.ID, res.Method, tc.wantID, tc.wantBy)
			}
		})
	}
}

func TestMixedErrorType(t *testing.T) {
	_, err := Resolve(testConfig(), map[string]meta.Info{"a.JPG": gr3x, "b.JPG": gr3}, "", "")
	// errors.As finds a specific error type even inside errors.Join.
	var mixed *MixedError
	if !errors.As(err, &mixed) {
		t.Fatalf("want *MixedError, got %T", err)
	}
	if mixed.Counts["GR3"] != 1 || mixed.Counts["GR3x"] != 1 {
		t.Errorf("counts = %v", mixed.Counts)
	}
}

func TestMatchLabel(t *testing.T) {
	cfg := testConfig()
	ids := func(cams []*config.Camera) string {
		var s []string
		for _, c := range cams {
			s = append(s, c.ID)
		}
		return strings.Join(s, ",")
	}
	if got := ids(MatchLabel(cfg, "INSTA360GO3")); got != "Go3" {
		t.Errorf("case-insensitive match: got %q", got)
	}
	// A camera without volume_label never matches, not even an empty label.
	if got := ids(MatchLabel(cfg, "")); got != "" {
		t.Errorf("empty label: got %q", got)
	}
}

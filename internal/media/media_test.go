package media

import "testing"

func TestIsMedia(t *testing.T) {
	def := NewSet(nil)
	custom := NewSet([]string{".xyz", "jpg"})
	cases := []struct {
		set  Set
		name string
		want bool
	}{
		{def, "DCIM/100RICOH/R0001.JPG", true},
		{def, "r0001.jpg", true},
		{def, "C0001.MP4", true},
		{def, "SDF01.X3F", true},
		{def, "MEDIAPRO.XML", false},
		{def, "README", false},
		{custom, "a.XYZ", true},
		{custom, "a.DNG", false},
	}
	for _, tc := range cases {
		if got := tc.set.IsMedia(tc.name); got != tc.want {
			t.Errorf("IsMedia(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

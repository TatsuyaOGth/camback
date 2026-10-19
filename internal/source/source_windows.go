package source

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// candidateRoots returns drive roots D:\ .. Z:\ that exist. The Go runtime
// disables the "insert a disk" dialog, so probing empty card readers is safe.
func candidateRoots() []string {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil
	}
	var out []string
	for i := 3; i < 26; i++ { // skip A:, B:, C:
		if mask&(1<<uint(i)) != 0 {
			out = append(out, string(rune('A'+i))+`:\`)
		}
	}
	return out
}

func volumeLabel(root string) string {
	vol := filepath.VolumeName(root)
	if vol == "" {
		return ""
	}
	p, err := windows.UTF16PtrFromString(vol + `\`)
	if err != nil {
		return ""
	}
	buf := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumeInformation(p, &buf[0], uint32(len(buf)), nil, nil, nil, nil, 0); err != nil {
		return ""
	}
	// Only use the label when root is the drive root itself.
	if !strings.EqualFold(filepath.Clean(root), vol+`\`) {
		return ""
	}
	return windows.UTF16ToString(buf)
}

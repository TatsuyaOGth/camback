// Package source finds memory cards and lists the files on them.
package source

import (
	"os"
	"path/filepath"
)

// Candidate is a mounted volume that looks like a camera card.
type Candidate struct {
	Root  string // mount point, e.g. "/Volumes/RICOH GR" or `E:\`
	Label string // volume label
}

// Detect returns mounted volumes that have a DCIM directory at their root.
// candidateRoots and volumeLabel are implemented per OS in
// source_darwin.go / source_windows.go / source_linux.go.
func Detect() []Candidate {
	var out []Candidate
	for _, root := range candidateRoots() {
		if HasDCIM(root) {
			out = append(out, Candidate{Root: root, Label: VolumeLabel(root)})
		}
	}
	return out
}

// HasDCIM reports whether root contains a DCIM directory.
func HasDCIM(root string) bool {
	fi, err := os.Stat(filepath.Join(root, "DCIM"))
	return err == nil && fi.IsDir()
}

// VolumeLabel returns the volume label for a mount point (or the base name
// of an arbitrary directory).
func VolumeLabel(root string) string {
	if l := volumeLabel(root); l != "" {
		return l
	}
	return filepath.Base(filepath.Clean(root))
}

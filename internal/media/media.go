// Package media decides which files are photos or videos, by extension.
package media

import (
	"path"
	"strings"
)

// DefaultExtensions is the built-in list of photo/video extensions.
var DefaultExtensions = []string{
	"JPG", "JPEG", "HIF", "HEIF", "HEIC",
	"DNG", "ARW", "SR2", "RAF", "X3F", "CR2", "CR3", "NEF", "NRW", "ORF", "RW2", "PEF",
	"MP4", "MOV", "MTS", "M2TS", "AVI", "INSV", "INSP", "LRV", "WAV",
}

// Set is a case-insensitive set of extensions (without the leading dot).
type Set map[string]struct{}

// NewSet builds a Set. A nil or empty list means DefaultExtensions.
func NewSet(exts []string) Set {
	if len(exts) == 0 {
		exts = DefaultExtensions
	}
	s := make(Set, len(exts))
	for _, e := range exts {
		s[strings.ToUpper(strings.TrimPrefix(strings.TrimSpace(e), "."))] = struct{}{}
	}
	return s
}

// IsMedia reports whether the file name has a media extension.
func (s Set) IsMedia(name string) bool {
	ext := path.Ext(name)
	if ext == "" {
		return false
	}
	_, ok := s[strings.ToUpper(ext[1:])]
	return ok
}

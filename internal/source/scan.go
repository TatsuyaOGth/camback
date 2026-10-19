package source

import (
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// File is a regular file on the card.
type File struct {
	Rel     string // path relative to the card root, always '/'-separated
	Abs     string // local absolute path (OS separators)
	Size    int64
	ModTime time.Time
}

// builtinExcludes are OS housekeeping entries (compared case-insensitively).
// Anything whose name starts with "." is excluded as well.
var builtinExcludes = []string{
	"System Volume Information",
	"$RECYCLE.BIN",
	"Thumbs.db",
	"desktop.ini",
}

// Excluded reports whether a card-relative path should be skipped.
// Patterns use path.Match syntax; a pattern without "/" is matched against
// each path element, otherwise against the whole relative path.
func Excluded(rel string, patterns []string) bool {
	elems := strings.Split(rel, "/")
	for _, e := range elems {
		if strings.HasPrefix(e, ".") {
			return true
		}
		for _, b := range builtinExcludes {
			if strings.EqualFold(e, b) {
				return true
			}
		}
	}
	for _, p := range patterns {
		if strings.Contains(p, "/") {
			if ok, _ := path.Match(p, rel); ok {
				return true
			}
			continue
		}
		for _, e := range elems {
			if ok, _ := path.Match(p, e); ok {
				return true
			}
		}
	}
	return false
}

// Scan lists every regular file under root that is not excluded, sorted by
// relative path. Symlinks and other special files are skipped.
func Scan(root string, excludes []string) ([]File, error) {
	var files []File
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if Excluded(rel, excludes) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		files = append(files, File{Rel: rel, Abs: p, Size: info.Size(), ModTime: info.ModTime()})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", root, err)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Rel < files[j].Rel })
	return files, nil
}

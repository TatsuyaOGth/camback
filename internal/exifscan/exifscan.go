// Package exifscan reads the EXIF Make/Model of a card's media files for
// camera detection. What it reads is kept in a per-card cache on this PC, so
// later runs read only the files that are new on the card. It can also read
// just a sample of the files.
package exifscan

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/TatsuyaOGth/camback/internal/meta"
	"github.com/TatsuyaOGth/camback/internal/source"
)

// cacheVersion changes when the cache file format does; older files are
// then ignored and rebuilt.
const cacheVersion = 1

// Dir returns the per-OS cache directory (e.g. ~/Library/Caches/camback on
// macOS). Like the logs, it is local to each PC.
func Dir() (string, error) {
	d, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "camback"), nil
}

// Options controls Scan.
type Options struct {
	// CacheDir holds the cache files. "" turns the cache off: nothing is
	// read from or written to disk.
	CacheDir string
	// Volume is the card's volume label; each label has its own cache file.
	Volume string
	// Sample reads only Sample() of the files the cache does not cover.
	Sample bool
	// Warn reports problems with the cache, which never stop the run.
	Warn func(string)
	// Read reads EXIF; meta.Read when nil (replaced in tests).
	Read func(ctx context.Context, dir string, rels []string) (map[string]meta.Info, error)
}

// Result is what Scan found.
type Result struct {
	// Infos holds the EXIF of every file that was checked, from the cache or
	// read now, keyed by card-relative path. A file exiftool could not read
	// is missing.
	Infos map[string]meta.Info
	// Cached counts the files whose EXIF came from the cache.
	Cached int
	// Skipped counts the files that were not checked because of Sample.
	Skipped int
}

// Scan returns the EXIF of the media files on the card at root.
func Scan(ctx context.Context, root string, files []source.File, opt Options) (*Result, error) {
	read := opt.Read
	if read == nil {
		read = meta.Read
	}
	warn := opt.Warn
	if warn == nil {
		warn = func(string) {}
	}

	var old map[string]entry
	var cachePath string
	if opt.CacheDir != "" {
		cachePath = filepath.Join(opt.CacheDir, fileName(opt.Volume))
		var err error
		if old, err = load(cachePath); err != nil {
			warn(fmt.Sprintf("ignoring the EXIF cache %s (it will be rebuilt): %v", cachePath, err))
		}
	}

	res := &Result{Infos: make(map[string]meta.Info, len(files))}
	keep := make(map[string]entry, len(files)) // the cache to save
	byRel := make(map[string]source.File, len(files))
	var uncached []string
	for _, f := range files {
		byRel[f.Rel] = f
		if e, ok := old[f.Rel]; ok && e.matches(f) {
			res.Infos[f.Rel] = e.info()
			keep[f.Rel] = e
			res.Cached++
			continue
		}
		uncached = append(uncached, f.Rel)
	}

	toRead := uncached
	if opt.Sample {
		toRead = Sample(uncached)
		res.Skipped = len(uncached) - len(toRead)
	}
	infos, err := read(ctx, root, toRead)
	if err != nil {
		return nil, err
	}
	added := 0
	for rel, info := range infos {
		res.Infos[rel] = info
		// Only files with a Make or Model are cached: exiftool gives the same
		// empty answer for a file without EXIF and for one it failed to read,
		// and a read error must not be remembered as "no EXIF".
		if f, ok := byRel[rel]; ok && info.Identified() {
			keep[rel] = newEntry(f, info)
			added++
		}
	}

	if cachePath != "" && (added > 0 || len(keep) != len(old)) {
		if err := save(cachePath, opt.Volume, keep); err != nil {
			warn(fmt.Sprintf("could not save the EXIF cache: %v", err))
		}
	}
	return res, nil
}

// Sample picks, in each folder, the first and the last file (by name) of
// each extension. Cameras usually write into folders of their own, so this
// still notices a card that another camera has written to.
func Sample(rels []string) []string {
	type group struct{ first, last string }
	groups := map[string]*group{}
	for _, rel := range rels {
		key := path.Dir(rel) + "\x00" + strings.ToUpper(path.Ext(rel))
		g := groups[key]
		if g == nil {
			groups[key] = &group{first: rel, last: rel}
			continue
		}
		if rel < g.first {
			g.first = rel
		}
		if rel > g.last {
			g.last = rel
		}
	}
	out := make([]string, 0, 2*len(groups))
	for _, g := range groups {
		out = append(out, g.first)
		if g.last != g.first {
			out = append(out, g.last)
		}
	}
	sort.Strings(out)
	return out
}

type entry struct {
	Size    int64  `json:"size"`
	ModTime int64  `json:"mtime"` // Unix nanoseconds
	Make    string `json:"make,omitempty"`
	Model   string `json:"model,omitempty"`
}

func newEntry(f source.File, info meta.Info) entry {
	return entry{Size: f.Size, ModTime: f.ModTime.UnixNano(), Make: info.Make, Model: info.Model}
}

// matches reports whether f is still the file the entry was read from.
func (e entry) matches(f source.File) bool {
	return e.Size == f.Size && e.ModTime == f.ModTime.UnixNano()
}

func (e entry) info() meta.Info { return meta.Info{Make: e.Make, Model: e.Model} }

type cacheFile struct {
	Version int              `json:"version"`
	Volume  string           `json:"volume"`
	Files   map[string]entry `json:"files"`
}

// fileName turns a volume label into a safe file name. The hash keeps labels
// that differ only in replaced characters apart.
func fileName(volume string) string {
	safe := []rune{}
	for _, r := range volume {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			safe = append(safe, r)
		default:
			safe = append(safe, '_')
		}
		if len(safe) == 40 {
			break
		}
	}
	sum := sha256.Sum256([]byte(volume))
	return "exif-" + string(safe) + "-" + hex.EncodeToString(sum[:4]) + ".json"
}

// load returns the cached entries; a missing file is an empty cache.
func load(p string) (map[string]entry, error) {
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c cacheFile
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	if c.Version != cacheVersion {
		return nil, nil // another format: start over without a warning
	}
	return c.Files, nil
}

// save writes the cache through a temporary file, so that an interrupted
// run never leaves a half-written cache behind.
func save(p, volume string, files map[string]entry) error {
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(cacheFile{Version: cacheVersion, Volume: volume, Files: files})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".exif-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after the rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// Package transport defines how camback talks to the destination.
//
// Remote paths always use '/' separators, regardless of the local OS, so
// they get their own type (Path) and are handled with the "path" package,
// never "path/filepath".
package transport

import (
	"errors"
	"io"
	"path"
	"time"
)

// Path is an absolute, '/'-separated path on the destination.
type Path string

// Join appends elements with '/' separators and cleans the result.
func (p Path) Join(elem ...string) Path {
	return Path(path.Join(append([]string{string(p)}, elem...)...))
}

// Dir returns the parent directory.
func (p Path) Dir() Path { return Path(path.Dir(string(p))) }

// Base returns the last element.
func (p Path) Base() string { return path.Base(string(p)) }

func (p Path) String() string { return string(p) }

// FileInfo is the subset of file metadata camback uses.
type FileInfo struct {
	Name    string
	Size    int64
	ModTime time.Time
	IsDir   bool
}

// ErrUnsupported is returned by SHA256 when the destination cannot hash
// files itself; callers then read the file back instead.
var ErrUnsupported = errors.ErrUnsupported

// Transport is implemented by the SFTP client and by the in-memory fake
// used in tests. Missing files are reported with errors that satisfy
// errors.Is(err, fs.ErrNotExist).
type Transport interface {
	Stat(p Path) (FileInfo, error)
	ReadDir(p Path) ([]FileInfo, error)
	MkdirAll(p Path) error
	// Create creates or truncates a file for writing.
	Create(p Path) (io.WriteCloser, error)
	Open(p Path) (io.ReadCloser, error)
	Chtimes(p Path, mtime time.Time) error
	// Rename moves from to to and must never replace an existing file;
	// it fails with an error satisfying errors.Is(err, fs.ErrExist).
	Rename(from, to Path) error
	Remove(p Path) error
	// SHA256 hashes the file on the destination side (hex string).
	SHA256(p Path) (string, error)
	Close() error
}

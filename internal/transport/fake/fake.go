// Package fake is an in-memory transport.Transport for tests.
package fake

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/TatsuyaOGth/camback/internal/transport"
)

type entry struct {
	data  []byte
	mtime time.Time
	dir   bool
	mode  fs.FileMode
}

// Default permissions of new entries: those of a server with umask 000.
const (
	fileMode fs.FileMode = 0o666
	dirMode  fs.FileMode = 0o777
)

// FS is a goroutine-safe in-memory file system.
type FS struct {
	mu      sync.Mutex
	entries map[transport.Path]*entry

	// HashSupported makes SHA256 work; otherwise it returns ErrUnsupported.
	HashSupported bool
	// HashCalls counts SHA256 calls.
	HashCalls int
	// OpenCalls counts Open calls (read-backs).
	OpenCalls int
	// WrongHash makes SHA256 return a hash of different content, to simulate
	// a wrong shell_path_map.
	WrongHash bool
	// IgnoreChmod makes Chmod succeed without changing anything, like some
	// servers do.
	IgnoreChmod bool
	// ChmodCalls lists the paths given to Chmod, in order.
	ChmodCalls []transport.Path
	// CorruptWrites flips the first byte of every file written via Create,
	// to simulate a transfer that went wrong.
	CorruptWrites bool
}

// New returns an empty FS containing only "/".
func New() *FS {
	return &FS{entries: map[transport.Path]*entry{"/": {dir: true, mode: dirMode}}}
}

func notExist(op string, p transport.Path) error {
	return &fs.PathError{Op: op, Path: string(p), Err: fs.ErrNotExist}
}

// Put stores a file, creating parent directories.
func (f *FS) Put(p transport.Path, data []byte, mtime time.Time) {
	_ = f.MkdirAll(p.Dir())
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries[p] = &entry{data: append([]byte(nil), data...), mtime: mtime, mode: fileMode}
}

// Get returns a file's content.
func (f *FS) Get(p transport.Path) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.entries[p]
	if !ok || e.dir {
		return nil, false
	}
	return e.data, true
}

// Paths lists every file (not directory), sorted.
func (f *FS) Paths() []transport.Path {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []transport.Path
	for p, e := range f.entries {
		if !e.dir {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (f *FS) info(p transport.Path, e *entry) transport.FileInfo {
	return transport.FileInfo{Name: p.Base(), Size: int64(len(e.data)), ModTime: e.mtime, IsDir: e.dir, Mode: e.mode}
}

func (f *FS) Stat(p transport.Path) (transport.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.entries[p]
	if !ok {
		return transport.FileInfo{}, notExist("stat", p)
	}
	return f.info(p, e), nil
}

func (f *FS) ReadDir(p transport.Path) ([]transport.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e, ok := f.entries[p]; !ok || !e.dir {
		return nil, notExist("readdir", p)
	}
	var out []transport.FileInfo
	for q, e := range f.entries {
		if q != p && q.Dir() == p {
			out = append(out, f.info(q, e))
		}
	}
	return out, nil
}

func (f *FS) MkdirAll(p transport.Path) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for q := p; ; q = q.Dir() {
		if e, ok := f.entries[q]; ok {
			if !e.dir {
				return fmt.Errorf("mkdir %s: not a directory", q)
			}
		} else {
			f.entries[q] = &entry{dir: true, mode: dirMode}
		}
		if q == "/" || q == "." {
			return nil
		}
	}
}

func (f *FS) Mkdir(p transport.Path) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.entries[p]; ok {
		return &fs.PathError{Op: "mkdir", Path: string(p), Err: fs.ErrExist}
	}
	if e, ok := f.entries[p.Dir()]; !ok || !e.dir {
		return notExist("mkdir", p)
	}
	f.entries[p] = &entry{dir: true, mode: dirMode}
	return nil
}

type writer struct {
	f *FS
	p transport.Path
	bytes.Buffer
}

func (w *writer) Close() error {
	data := w.Bytes()
	if w.f.CorruptWrites && len(data) > 0 {
		data[0] ^= 0xFF
	}
	w.f.mu.Lock()
	defer w.f.mu.Unlock()
	w.f.entries[w.p] = &entry{data: data, mtime: time.Now(), mode: fileMode}
	return nil
}

func (f *FS) Create(p transport.Path) (io.WriteCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e, ok := f.entries[p.Dir()]; !ok || !e.dir {
		return nil, notExist("create", p)
	}
	if e, ok := f.entries[p]; ok && e.dir {
		return nil, fmt.Errorf("create %s: is a directory", p)
	}
	f.entries[p] = &entry{mtime: time.Now(), mode: fileMode}
	return &writer{f: f, p: p}, nil
}

func (f *FS) Open(p transport.Path) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.OpenCalls++
	e, ok := f.entries[p]
	if !ok || e.dir {
		return nil, notExist("open", p)
	}
	return io.NopCloser(bytes.NewReader(e.data)), nil
}

func (f *FS) Chtimes(p transport.Path, mtime time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.entries[p]
	if !ok {
		return notExist("chtimes", p)
	}
	e.mtime = mtime
	return nil
}

func (f *FS) Chmod(p transport.Path, mode fs.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ChmodCalls = append(f.ChmodCalls, p)
	e, ok := f.entries[p]
	if !ok {
		return notExist("chmod", p)
	}
	if !f.IgnoreChmod {
		e.mode = mode.Perm()
	}
	return nil
}

func (f *FS) Rename(from, to transport.Path) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.entries[from]
	if !ok {
		return notExist("rename", from)
	}
	if _, exists := f.entries[to]; exists {
		return &fs.PathError{Op: "rename", Path: string(to), Err: fs.ErrExist}
	}
	f.entries[to] = e
	delete(f.entries, from)
	return nil
}

func (f *FS) Remove(p transport.Path) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.entries[p]; !ok {
		return notExist("remove", p)
	}
	prefix := string(p) + "/"
	for q := range f.entries {
		if strings.HasPrefix(string(q), prefix) {
			return fmt.Errorf("remove %s: directory not empty", p)
		}
	}
	delete(f.entries, p)
	return nil
}

func (f *FS) SHA256(p transport.Path) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.HashCalls++
	if !f.HashSupported {
		return "", transport.ErrUnsupported
	}
	e, ok := f.entries[p]
	if !ok || e.dir {
		return "", notExist("sha256", p)
	}
	data := e.data
	if f.WrongHash {
		data = append(append([]byte(nil), data...), 'x')
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (f *FS) Close() error { return nil }

// Compile-time check that *FS implements transport.Transport.
var _ transport.Transport = (*FS)(nil)

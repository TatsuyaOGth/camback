// Package sftp implements transport.Transport over SSH/SFTP.
package sftp

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"

	psftp "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/TatsuyaOGth/camback/internal/transport"
)

// Client is an SFTP connection plus an optional way to run shell commands
// (used for sha256sum on the NAS).
type Client struct {
	fs       *psftp.Client
	ssh      *ssh.Client // nil in tests
	exec     func(cmd string) ([]byte, error)
	shellMap map[string]string
}

// newClient wraps an SFTP client. exec may be nil (no remote hashing).
func newClient(fs *psftp.Client, exec func(string) ([]byte, error), shellMap map[string]string) *Client {
	return &Client{fs: fs, exec: exec, shellMap: shellMap}
}

var _ transport.Transport = (*Client)(nil)

func toInfo(fi fs.FileInfo) transport.FileInfo {
	return transport.FileInfo{Name: fi.Name(), Size: fi.Size(), ModTime: fi.ModTime(), IsDir: fi.IsDir(), Mode: fi.Mode().Perm()}
}

func (c *Client) Stat(p transport.Path) (transport.FileInfo, error) {
	fi, err := c.fs.Stat(string(p))
	if err != nil {
		return transport.FileInfo{}, err
	}
	return toInfo(fi), nil
}

func (c *Client) ReadDir(p transport.Path) ([]transport.FileInfo, error) {
	list, err := c.fs.ReadDir(string(p))
	if err != nil {
		return nil, err
	}
	out := make([]transport.FileInfo, len(list))
	for i, fi := range list {
		out[i] = toInfo(fi)
	}
	return out, nil
}

func (c *Client) MkdirAll(p transport.Path) error { return c.fs.MkdirAll(string(p)) }

func (c *Client) Mkdir(p transport.Path) error { return c.fs.Mkdir(string(p)) }

// remoteFile makes io.Copy use concurrent SFTP writes.
type remoteFile struct{ *psftp.File }

func (f remoteFile) ReadFrom(r io.Reader) (int64, error) {
	return f.File.ReadFromWithConcurrency(r, 0)
}

func (c *Client) Create(p transport.Path) (io.WriteCloser, error) {
	f, err := c.fs.Create(string(p))
	if err != nil {
		return nil, err
	}
	return remoteFile{f}, nil
}

func (c *Client) Open(p transport.Path) (io.ReadCloser, error) {
	f, err := c.fs.Open(string(p))
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (c *Client) Chtimes(p transport.Path, mtime time.Time) error {
	return c.fs.Chtimes(string(p), mtime, mtime)
}

func (c *Client) Chmod(p transport.Path, mode fs.FileMode) error {
	return c.fs.Chmod(string(p), mode)
}

// Rename never replaces an existing file. It checks first, then uses the
// classic SSH_FXP_RENAME, which OpenSSH-based servers (including Synology)
// refuse when the target exists. posix-rename is deliberately not used,
// because it overwrites.
func (c *Client) Rename(from, to transport.Path) error {
	if _, err := c.fs.Lstat(string(to)); err == nil {
		return &fs.PathError{Op: "rename", Path: string(to), Err: fs.ErrExist}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return c.fs.Rename(string(from), string(to))
}

func (c *Client) Remove(p transport.Path) error { return c.fs.Remove(string(p)) }

// SHA256 runs sha256sum through the SSH shell, translating the SFTP path
// with shell_path_map.
func (c *Client) SHA256(p transport.Path) (string, error) {
	if c.exec == nil {
		return "", transport.ErrUnsupported
	}
	shellPath := MapPath(c.shellMap, string(p))
	out, err := c.exec("sha256sum -- " + ShellQuote(shellPath))
	if err != nil {
		return "", fmt.Errorf("sha256sum %s: %w", shellPath, err)
	}
	sum, _, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	sum = strings.TrimPrefix(sum, `\`) // sha256sum escapes names with odd characters
	if b, err := hex.DecodeString(sum); err != nil || len(b) != 32 {
		return "", fmt.Errorf("sha256sum %s: unexpected output %q", shellPath, out)
	}
	return strings.ToLower(sum), nil
}

func (c *Client) Close() error {
	err := c.fs.Close()
	if c.ssh != nil {
		if e := c.ssh.Close(); err == nil {
			err = e
		}
	}
	return err
}

// MapPath translates an SFTP path to the shell's view using the longest
// matching prefix, compared element by element ("/photo" does not match
// "/photos"). Paths that match nothing are returned unchanged.
func MapPath(m map[string]string, p string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, k := range keys {
		prefix := path.Clean(k)
		if p == prefix {
			return path.Clean(m[k])
		}
		if prefix == "/" {
			return path.Join(m[k], p)
		}
		if strings.HasPrefix(p, prefix+"/") {
			return path.Join(m[k], p[len(prefix):])
		}
	}
	return p
}

// ShellQuote quotes s for a POSIX shell: wrap in single quotes and replace
// each ' with '\”.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Package config loads and validates the camback TOML configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// EnvConfig is the environment variable that overrides the default config location.
const EnvConfig = "CAMBACK_CONFIG"

// Verify modes.
const (
	VerifyHash = "hash"
	VerifySize = "size"
)

// What to do with a file that conflicts with the destination (a different
// photo or video with the same name, or a timestamped name that is taken).
const (
	OnConflictAbort  = "abort"  // stop before transferring anything
	OnConflictSkip   = "skip"   // leave the file on the card only
	OnConflictRename = "rename" // save it next to the other one under a timestamped name
)

// Parallel transfers.
const (
	DefaultJobs = 4
	// MaxJobs keeps the SSH sessions camback opens (one for SFTP, one per
	// sha256sum in progress) below OpenSSH's default MaxSessions of 10.
	MaxJobs = 8
)

// Config is the whole configuration file.
type Config struct {
	Remotes  map[string]Remote `toml:"remote"`
	Defaults Defaults          `toml:"defaults"`
	Cameras  []Camera          `toml:"camera"`
}

// Remote is an SFTP destination.
type Remote struct {
	Host         string `toml:"host"`
	Port         int    `toml:"port"`
	User         string `toml:"user"`
	IdentityFile string `toml:"identity_file"`
	// ShellPathMap maps SFTP path prefixes to the paths seen by the SSH shell
	// (used when running sha256sum on the NAS).
	ShellPathMap map[string]string `toml:"shell_path_map"`
	// FileMode and DirMode are the permissions given to the files and the
	// directories camback creates (e.g. 0o644). nil leaves them to the
	// server; a pointer, so that an explicit 0 is an error, not "unset".
	FileMode *fs.FileMode `toml:"file_mode"`
	DirMode  *fs.FileMode `toml:"dir_mode"`
}

// Defaults holds settings shared by every camera.
type Defaults struct {
	Verify          string   `toml:"verify"`
	Jobs            int      `toml:"jobs"` // files transferred at the same time
	OnConflict      string   `toml:"on_conflict"`
	MediaExtensions []string `toml:"media_extensions"`
	Exclude         []string `toml:"exclude"`
}

// Camera is one camera profile.
type Camera struct {
	ID          string  `toml:"id"`
	Match       []Match `toml:"match"`
	VolumeLabel string  `toml:"volume_label"`
	Remote      string  `toml:"remote"`
	Dest        string  `toml:"dest"`
}

// Match is an exact EXIF Make/Model pair.
type Match struct {
	Make  string `toml:"make"`
	Model string `toml:"model"`
}

// ValidOnConflict reports whether s is one of the OnConflict values.
func ValidOnConflict(s string) bool {
	switch s {
	case OnConflictAbort, OnConflictSkip, OnConflictRename:
		return true
	}
	return false
}

// DefaultPath returns the per-OS default config file location.
func DefaultPath() (string, error) {
	if runtime.GOOS == "windows" {
		dir, err := os.UserConfigDir() // %AppData%
		if err != nil {
			return "", err
		}
		return filepath.Join(dir, "camback", "config.toml"), nil
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "camback", "config.toml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "camback", "config.toml"), nil
}

// ResolvePath picks the config path: flag > CAMBACK_CONFIG > default.
func ResolvePath(flagValue string) (string, error) {
	if flagValue != "" {
		return ExpandHome(flagValue)
	}
	if env := os.Getenv(EnvConfig); env != "" {
		return ExpandHome(env)
	}
	return DefaultPath()
}

// ExpandHome replaces a leading "~" with the current user's home directory.
func ExpandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") && !strings.HasPrefix(p, `~\`) {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, filepath.FromSlash(p[1:])), nil
}

// Load reads, decodes and validates the config file at p.
func Load(p string) (*Config, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return cfg, nil
}

// Parse decodes and validates TOML data. Unknown keys are rejected so that
// typos do not silently fall back to defaults.
func Parse(data []byte) (*Config, error) {
	// Defaults that 0 or "" cannot stand for are set before decoding, so an
	// explicit 0 still fails validation.
	cfg := Config{Defaults: Defaults{Jobs: DefaultJobs}}
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		var sm *toml.StrictMissingError
		if errors.As(err, &sm) {
			return nil, fmt.Errorf("unknown config keys:\n%s", sm.String())
		}
		return nil, err
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Defaults.Verify == "" {
		c.Defaults.Verify = VerifyHash
	}
	if c.Defaults.OnConflict == "" {
		c.Defaults.OnConflict = OnConflictAbort
	}
	for name, r := range c.Remotes {
		if r.Port == 0 {
			r.Port = 22
		}
		c.Remotes[name] = r
	}
	for i := range c.Cameras {
		if d := c.Cameras[i].Dest; strings.HasPrefix(d, "/") {
			c.Cameras[i].Dest = path.Clean(d)
		}
	}
}

// Validate checks cross references and value formats. All problems are
// reported together.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	switch c.Defaults.Verify {
	case VerifyHash, VerifySize:
	default:
		add("defaults.verify: must be %q or %q, got %q", VerifyHash, VerifySize, c.Defaults.Verify)
	}
	if !ValidOnConflict(c.Defaults.OnConflict) {
		add("defaults.on_conflict: must be %q, %q or %q, got %q", OnConflictAbort, OnConflictSkip, OnConflictRename, c.Defaults.OnConflict)
	}
	if j := c.Defaults.Jobs; j < 1 || j > MaxJobs {
		add("defaults.jobs: must be between 1 and %d, got %d", MaxJobs, j)
	}
	for _, e := range c.Defaults.MediaExtensions {
		if strings.TrimSpace(e) == "" {
			add("defaults.media_extensions: empty extension")
		}
	}
	for _, pat := range c.Defaults.Exclude {
		if _, err := path.Match(pat, ""); err != nil {
			add("defaults.exclude: bad pattern %q: %v", pat, err)
		}
	}

	names := make([]string, 0, len(c.Remotes))
	for name := range c.Remotes {
		names = append(names, name)
	}
	sort.Strings(names) // map iteration order is random in Go; sort for stable messages
	for _, name := range names {
		r := c.Remotes[name]
		if r.Host == "" {
			add("remote.%s.host: required", name)
		}
		if r.User == "" {
			add("remote.%s.user: required", name)
		}
		if r.Port < 1 || r.Port > 65535 {
			add("remote.%s.port: out of range: %d", name, r.Port)
		}
		for k, v := range r.ShellPathMap {
			if !strings.HasPrefix(k, "/") || !strings.HasPrefix(v, "/") {
				add("remote.%s.shell_path_map: %q = %q: both sides must be absolute paths starting with /", name, k, v)
			}
		}
		checkMode := func(key string, m *fs.FileMode, owner fs.FileMode, ownerName string) {
			switch {
			case m == nil:
			case *m > fs.ModePerm:
				// file_mode = 644 (no 0o) is decimal 644, i.e. 0o1204.
				add("remote.%s.%s: %d is not a permission from 0o000 to 0o777; write it in octal, like 0o%03o", name, key, uint32(*m), uint32(owner))
			case *m&owner != owner:
				add("remote.%s.%s: 0o%03o must give the owner %s (0o%03o)", name, key, uint32(*m), ownerName, uint32(owner))
			}
		}
		checkMode("file_mode", r.FileMode, 0o600, "read and write")
		checkMode("dir_mode", r.DirMode, 0o700, "read, write and search")
	}

	seen := map[string]bool{}
	matchOwner := map[Match]string{} // trimmed make/model -> camera label
	for i, cam := range c.Cameras {
		label := fmt.Sprintf("camera[%d]", i)
		if cam.ID != "" {
			label = fmt.Sprintf("camera %q", cam.ID)
		}
		switch {
		case cam.ID == "":
			add("%s: id is required", label)
		case seen[cam.ID]:
			add("%s: duplicate id", label)
		}
		seen[cam.ID] = true
		if len(cam.Match) == 0 && cam.VolumeLabel == "" {
			add("%s: needs at least one match entry or volume_label", label)
		}
		for j, m := range cam.Match {
			if strings.TrimSpace(m.Make) == "" || strings.TrimSpace(m.Model) == "" {
				add("%s: match[%d]: make and model are both required", label, j)
				continue
			}
			// The same Make/Model in two cameras would silently send every
			// card to whichever camera comes first.
			key := Match{Make: strings.TrimSpace(m.Make), Model: strings.TrimSpace(m.Model)}
			if owner, ok := matchOwner[key]; ok && owner != label {
				add("%s: match make = %q, model = %q is already used by %s", label, key.Make, key.Model, owner)
			}
			matchOwner[key] = label
		}
		if _, ok := c.Remotes[cam.Remote]; !ok {
			add("%s: remote %q is not defined", label, cam.Remote)
		}
		if !strings.HasPrefix(cam.Dest, "/") {
			add("%s: dest %q must be an absolute SFTP path starting with /", label, cam.Dest)
		}
	}
	return errors.Join(errs...)
}

// Camera returns the camera profile with the given id.
func (c *Config) Camera(id string) (*Camera, bool) {
	for i := range c.Cameras {
		if c.Cameras[i].ID == id {
			return &c.Cameras[i], true
		}
	}
	return nil, false
}

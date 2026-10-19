// Package applog writes one JSON Lines log file per run to the OS log
// directory (kept local to each PC, not in the shared config folder).
package applog

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// Dir returns the per-OS log directory.
func Dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Logs", "camback"), nil
	case "windows":
		if d := os.Getenv("LocalAppData"); d != "" {
			return filepath.Join(d, "camback", "logs"), nil
		}
		return filepath.Join(home, "AppData", "Local", "camback", "logs"), nil
	default:
		if d := os.Getenv("XDG_STATE_HOME"); d != "" {
			return filepath.Join(d, "camback"), nil
		}
		return filepath.Join(home, ".local", "state", "camback"), nil
	}
}

// Open creates a new log file. Every record carries the run id.
func Open() (logger *slog.Logger, path string, closeFn func() error, err error) {
	dir, err := Dir()
	if err != nil {
		return nil, "", nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, "", nil, err
	}
	b := make([]byte, 3)
	rand.Read(b)
	id := time.Now().Format("20060102-150405") + "-" + hex.EncodeToString(b)
	path = filepath.Join(dir, "camback-"+id+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, "", nil, err
	}
	logger = slog.New(slog.NewJSONHandler(f, nil)).With("run", id)
	return logger, path, f.Close, nil
}

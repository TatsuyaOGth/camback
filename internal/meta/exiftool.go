// Package meta reads EXIF Make/Model through the external exiftool command.
package meta

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Info is the metadata camback needs from one file.
type Info struct {
	Make  string
	Model string
}

// Identified reports whether the file carried a camera Make or Model.
func (i Info) Identified() bool { return i.Make != "" || i.Model != "" }

// ErrNotFound is returned when exiftool is not installed.
var ErrNotFound = errors.New("exiftool not found in PATH (install it: https://exiftool.org/)")

// Check verifies that exiftool can be executed.
func Check() error {
	if _, err := exec.LookPath("exiftool"); err != nil {
		return ErrNotFound
	}
	return nil
}

// Version returns exiftool's version (e.g. "13.55") and where it was found.
func Version(ctx context.Context) (version, path string, err error) {
	path, err = exec.LookPath("exiftool")
	if err != nil {
		return "", "", ErrNotFound
	}
	out, err := exec.CommandContext(ctx, path, "-ver").Output()
	if err != nil {
		return "", path, fmt.Errorf("exiftool -ver: %w", err)
	}
	return strings.TrimSpace(string(out)), path, nil
}

// flexString accepts both JSON strings and numbers, because exiftool prints
// numeric-looking values (e.g. a model named "5") as numbers.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*f = flexString(s)
		return nil
	}
	*f = flexString(strings.TrimSpace(string(b)))
	return nil
}

type record struct {
	SourceFile string     `json:"SourceFile"`
	Make       flexString `json:"Make"`
	Model      flexString `json:"Model"`
}

// Read returns Make/Model for files given relative to dir ('/'-separated).
// File names are passed on stdin (-@ -), so there is no command-line length
// limit. Files exiftool cannot read are simply missing from the result.
func Read(ctx context.Context, dir string, rels []string) (map[string]Info, error) {
	out := make(map[string]Info, len(rels))
	if len(rels) == 0 {
		return out, nil
	}
	if err := Check(); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "exiftool",
		"-json", "-fast2", "-charset", "filename=utf8",
		"-Make", "-Model", "-@", "-")
	cmd.Dir = dir
	// "./" keeps a file named like "-foo.JPG" from being read as an option.
	var args strings.Builder
	for _, rel := range rels {
		args.WriteString("./" + rel + "\n")
	}
	cmd.Stdin = strings.NewReader(args.String())
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	// exiftool exits 1 when some files could not be read but still prints
	// JSON for the others, so only fail when there is no usable output.
	if stdout.Len() == 0 {
		if runErr != nil {
			return nil, fmt.Errorf("exiftool: %v: %s", runErr, strings.TrimSpace(stderr.String()))
		}
		return out, nil
	}
	var recs []record
	if err := json.Unmarshal(stdout.Bytes(), &recs); err != nil {
		return nil, fmt.Errorf("exiftool: parse output: %w", err)
	}
	for _, r := range recs {
		out[strings.TrimPrefix(r.SourceFile, "./")] = Info{
			Make:  strings.TrimSpace(string(r.Make)),
			Model: strings.TrimSpace(string(r.Model)),
		}
	}
	return out, nil
}

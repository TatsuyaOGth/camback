// Package hashing computes SHA-256 sums of local and remote files.
package hashing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/TatsuyaOGth/camback/internal/transport"
)

// ContextReader stops reading once ctx is cancelled (Ctrl-C), so long
// copies and hashes end between reads instead of running to completion.
type ContextReader struct {
	Ctx context.Context
	R   io.Reader
}

func (c *ContextReader) Read(p []byte) (int, error) {
	if err := c.Ctx.Err(); err != nil {
		return 0, err
	}
	return c.R.Read(p)
}

// contextWriter is the writing-side counterpart of ContextReader. Checking
// on the write side keeps io.Copy able to use the source's WriteTo method,
// which an SFTP file implements with concurrent reads.
type contextWriter struct {
	ctx context.Context
	w   io.Writer
}

func (c *contextWriter) Write(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.w.Write(p)
}

// Reader hashes everything read from r.
func Reader(ctx context.Context, r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(&contextWriter{ctx: ctx, w: h}, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// File hashes a local file.
func File(ctx context.Context, p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return Reader(ctx, f)
}

// ReadBack hashes a remote file by downloading it.
func ReadBack(ctx context.Context, t transport.Transport, p transport.Path) (string, error) {
	r, err := t.Open(p)
	if err != nil {
		return "", err
	}
	defer r.Close()
	return Reader(ctx, r)
}

// RemoteHasher hashes destination files for the planner, preferring the
// destination's own sha256sum (no download) when it can be trusted.
//
// The first file is hashed both ways: if sha256sum agrees with the
// downloaded content, shell_path_map is right and later files use
// sha256sum only. Otherwise every file is downloaded. This check needs no
// write access, so it also works for --dry-run. Not safe for concurrent use.
type RemoteHasher struct {
	t       transport.Transport
	warn    func(string)
	checked bool
	trusted bool
}

// NewRemoteHasher returns a hasher for t. warn receives fallback notices.
func NewRemoteHasher(t transport.Transport, warn func(string)) *RemoteHasher {
	if warn == nil {
		warn = func(string) {}
	}
	return &RemoteHasher{t: t, warn: warn}
}

// Sum returns the SHA-256 of a destination file.
func (h *RemoteHasher) Sum(ctx context.Context, p transport.Path) (string, error) {
	if !h.checked {
		h.checked = true
		return h.firstSum(ctx, p)
	}
	if h.trusted {
		if sum, err := h.t.SHA256(p); err == nil {
			return sum, nil
		}
		// A one-off failure (e.g. a dropped session): download this file.
	}
	return ReadBack(ctx, h.t, p)
}

func (h *RemoteHasher) firstSum(ctx context.Context, p transport.Path) (string, error) {
	sum, err := h.t.SHA256(p)
	if err != nil {
		if !errors.Is(err, transport.ErrUnsupported) {
			h.warn(fmt.Sprintf("sha256sum on the destination failed (%v); comparing files by download (slower). Check shell_path_map", err))
		}
		return ReadBack(ctx, h.t, p)
	}
	downloaded, err := ReadBack(ctx, h.t, p)
	if err != nil {
		return "", err
	}
	if downloaded != sum {
		h.warn("sha256sum on the destination does not match the downloaded file; comparing files by download (slower). Check shell_path_map")
		return downloaded, nil
	}
	h.trusted = true
	return downloaded, nil
}

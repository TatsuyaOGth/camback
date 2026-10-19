// Package execute carries out a plan: write each file under a temporary
// name, verify it, then move it into place without ever replacing a file.
package execute

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"

	"github.com/TatsuyaOGth/camback/internal/config"
	"github.com/TatsuyaOGth/camback/internal/hashing"
	"github.com/TatsuyaOGth/camback/internal/plan"
	"github.com/TatsuyaOGth/camback/internal/transport"
)

// TempSuffix marks files that are still being written.
const TempSuffix = ".camback-part"

// ErrConflict means a file appeared at the destination after planning.
var ErrConflict = errors.New("destination file appeared during transfer")

// Progress receives transfer progress. Implementations live in the CLI.
type Progress interface {
	Start(files int, bytes int64)
	// File is called before each file; the returned writer receives every
	// byte read from the card.
	File(index int, it plan.Item) io.Writer
	// Done is called once at the end; ok is false when the run stopped early.
	Done(ok bool)
}

// Options configures Run.
type Options struct {
	Verify   string // config.VerifyHash or config.VerifySize
	Logger   *slog.Logger
	Progress Progress
	Warn     func(msg string)
}

// Result summarizes a run.
type Result struct {
	Files        int
	Bytes        int64
	VerifyMethod string // "sha256sum on destination", "read-back", "size"
}

// Run transfers every NEW and RENAME item of p, in order. It stops at the
// first error; files already moved into place are complete and verified.
func Run(ctx context.Context, t transport.Transport, p *plan.Plan, opt Options) (*Result, error) {
	if opt.Logger == nil {
		opt.Logger = slog.New(slog.DiscardHandler)
	}
	if opt.Warn == nil {
		opt.Warn = func(string) {}
	}
	if opt.Progress == nil {
		opt.Progress = nopProgress{}
	}
	if len(p.Filter(plan.Conflict)) > 0 {
		return nil, errors.New("plan has conflicts; refusing to run")
	}
	items := p.Filter(plan.New, plan.Rename)
	res := &Result{VerifyMethod: "size"}
	if len(items) == 0 {
		return res, nil
	}

	r := &runner{t: t, opt: opt}
	if opt.Verify == config.VerifyHash {
		if err := t.MkdirAll(p.Dest); err != nil {
			return res, fmt.Errorf("mkdir %s: %w", p.Dest, err)
		}
		r.remoteHash, res.VerifyMethod = r.chooseRemoteHash(ctx, p.Dest)
	}

	var total int64
	for _, it := range items {
		total += it.File.Size
	}
	opt.Progress.Start(len(items), total)
	for i, it := range items {
		if err := ctx.Err(); err != nil {
			opt.Progress.Done(false)
			return res, err
		}
		if err := r.transfer(ctx, i, it); err != nil {
			opt.Progress.Done(false)
			opt.Logger.Error("transfer failed", "file", it.File.Rel, "dest", it.Dest.String(), "error", err.Error())
			return res, fmt.Errorf("%s: %w", it.File.Rel, err)
		}
		res.Files++
		res.Bytes += it.File.Size
	}
	opt.Progress.Done(true)
	return res, nil
}

type runner struct {
	t          transport.Transport
	opt        Options
	remoteHash func(context.Context, transport.Path) (string, error) // nil when Verify=size
}

// chooseRemoteHash checks with a probe file whether sha256sum on the
// destination sees the same file as SFTP (i.e. shell_path_map is right).
// If not, it falls back to downloading files to hash them.
func (r *runner) chooseRemoteHash(ctx context.Context, dir transport.Path) (func(context.Context, transport.Path) (string, error), string) {
	readBack := func(ctx context.Context, p transport.Path) (string, error) { return hashing.ReadBack(ctx, r.t, p) }

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return readBack, "read-back"
	}
	probe := dir.Join(".camback-probe-" + hex.EncodeToString(buf[:8]))
	defer r.t.Remove(probe)

	w, err := r.t.Create(probe)
	if err == nil {
		_, err = w.Write(buf)
		if cerr := w.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		r.opt.Warn(fmt.Sprintf("cannot write probe file %s: %v; verifying by read-back", probe, err))
		return readBack, "read-back"
	}
	sum := sha256.Sum256(buf)
	got, err := r.t.SHA256(probe)
	switch {
	case err == nil && got == hex.EncodeToString(sum[:]):
		return func(_ context.Context, p transport.Path) (string, error) { return r.t.SHA256(p) }, "sha256sum on destination"
	case errors.Is(err, transport.ErrUnsupported):
	case err != nil:
		r.opt.Warn(fmt.Sprintf("sha256sum on the destination failed (%v); check shell_path_map. Verifying by read-back (slower)", err))
	default:
		r.opt.Warn("sha256sum on the destination returned a different hash for the probe file; check shell_path_map. Verifying by read-back (slower)")
	}
	return readBack, "read-back"
}

func tempPath(dest transport.Path) transport.Path {
	return dest.Dir().Join("." + dest.Base() + TempSuffix)
}

func (r *runner) transfer(ctx context.Context, i int, it plan.Item) (err error) {
	tmp := tempPath(it.Dest)
	if err := r.t.MkdirAll(it.Dest.Dir()); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	defer func() {
		if err != nil {
			r.t.Remove(tmp) // best effort; a leftover .camback-part is overwritten next time
		}
	}()

	src, err := os.Open(it.File.Abs)
	if err != nil {
		return err
	}
	defer src.Close()

	w, err := r.t.Create(tmp)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	// Read the card once: the same bytes go to the NAS, the hash and the
	// progress bar.
	h := sha256.New()
	reader := io.TeeReader(&hashing.ContextReader{Ctx: ctx, R: src}, io.MultiWriter(h, r.opt.Progress.File(i, it)))
	n, err := io.Copy(w, reader)
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if n != it.File.Size {
		return fmt.Errorf("read %d bytes from card, expected %d (file changed?)", n, it.File.Size)
	}
	localSum := hex.EncodeToString(h.Sum(nil))

	if err := r.t.Chtimes(tmp, it.File.ModTime); err != nil {
		return fmt.Errorf("set mtime: %w", err)
	}
	if err := r.verify(ctx, tmp, it.File.Size, localSum); err != nil {
		return err
	}

	if err := r.t.Rename(tmp, it.Dest); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%w: %s", ErrConflict, it.Dest)
		}
		return fmt.Errorf("rename to %s: %w", it.Dest, err)
	}
	r.opt.Logger.Info("transferred",
		"file", it.File.Rel, "dest", it.Dest.String(), "action", it.Action.String(),
		"bytes", it.File.Size, "sha256", localSum)
	return nil
}

func (r *runner) verify(ctx context.Context, tmp transport.Path, size int64, localSum string) error {
	fi, err := r.t.Stat(tmp)
	if err != nil {
		return fmt.Errorf("verify: %w", err)
	}
	if fi.Size != size {
		return fmt.Errorf("verify: destination has %d bytes, expected %d", fi.Size, size)
	}
	if r.remoteHash == nil {
		return nil
	}
	remoteSum, err := r.remoteHash(ctx, tmp)
	if err != nil {
		return fmt.Errorf("verify: %w", err)
	}
	if remoteSum != localSum {
		return fmt.Errorf("verify: SHA-256 mismatch (card %s, destination %s)", localSum, remoteSum)
	}
	return nil
}

type nopProgress struct{}

func (nopProgress) Start(int, int64)              {}
func (nopProgress) File(int, plan.Item) io.Writer { return io.Discard }
func (nopProgress) Done(bool)                     {}

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
	"sync"

	"golang.org/x/sync/errgroup"

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
//
// With Options.Jobs > 1, File and FileDone are called from several
// goroutines at once, and the writers File returns are written to
// concurrently.
type Progress interface {
	Start(files int, bytes int64)
	// File is called when a file starts; the returned writer receives every
	// byte read from the card.
	File(index int, it plan.Item) io.Writer
	// FileDone is called when a file ends; ok is false if it failed or was
	// cancelled.
	FileDone(index int, ok bool)
	// Done is called once at the end; ok is false when the run stopped early.
	Done(ok bool)
}

// Options configures Run.
type Options struct {
	Verify   string // config.VerifyHash or config.VerifySize
	Jobs     int    // files transferred at the same time; less than 1 means 1
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

// Run transfers every NEW and RENAME item of p, up to opt.Jobs files at a
// time, starting them in plan order. At the first error it starts no more
// files and cancels the ones in progress, whose temporary files are
// removed; one already verified by then is still moved into place. Files
// moved into place are complete and verified.
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
	// Two files never share a destination: parallel transfers would share
	// its temporary file. (A file on the card can carry the timestamped
	// name another one is renamed to.)
	seen := map[transport.Path]string{}
	for _, it := range items {
		if other, ok := seen[it.Dest]; ok {
			return res, fmt.Errorf("%w: %s and %s would both be saved as %s", ErrConflict, other, it.File.Rel, it.Dest)
		}
		seen[it.Dest] = it.File.Rel
	}

	r := &runner{t: t, opt: opt}
	if opt.Verify == config.VerifyHash {
		if err := t.MkdirAll(p.Dest); err != nil {
			return res, fmt.Errorf("mkdir %s: %w", p.Dest, err)
		}
		r.remoteHash, res.VerifyMethod = r.chooseRemoteHash(ctx, p.Dest)
	}

	// Create the directories first: parallel transfers do not race to
	// create the same one, and each file needs one round trip less.
	made := map[transport.Path]bool{}
	for _, it := range items {
		if d := it.Dest.Dir(); !made[d] {
			made[d] = true
			if err := ctx.Err(); err != nil {
				return res, err
			}
			if err := t.MkdirAll(d); err != nil {
				return res, fmt.Errorf("mkdir %s: %w", d, err)
			}
		}
	}

	var total int64
	for _, it := range items {
		total += it.File.Size
	}
	opt.Progress.Start(len(items), total)

	var mu sync.Mutex // guards res
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(max(opt.Jobs, 1))
	stopped := false
	for i, it := range items {
		if gctx.Err() != nil { // a transfer failed, or the run was interrupted
			stopped = true
			break
		}
		g.Go(func() error {
			if err := gctx.Err(); err != nil {
				return err // stopped while this file waited for a free slot
			}
			err := r.transfer(gctx, it, opt.Progress.File(i, it))
			opt.Progress.FileDone(i, err == nil)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					opt.Logger.Warn("transfer cancelled", "file", it.File.Rel, "dest", it.Dest.String())
				} else {
					opt.Logger.Error("transfer failed", "file", it.File.Rel, "dest", it.Dest.String(), "error", err.Error())
				}
				return fmt.Errorf("%s: %w", it.File.Rel, err)
			}
			mu.Lock()
			res.Files++
			res.Bytes += it.File.Size
			mu.Unlock()
			return nil
		})
	}
	err := g.Wait() // the first error; the others were caused by cancelling
	if err == nil && stopped {
		err = ctx.Err() // interrupted between files
	}
	opt.Progress.Done(err == nil)
	return res, err
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

// transfer copies one file; progress receives every byte read from the card.
func (r *runner) transfer(ctx context.Context, it plan.Item, progress io.Writer) (err error) {
	tmp := tempPath(it.Dest)
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
	reader := io.TeeReader(&hashing.ContextReader{Ctx: ctx, R: src}, io.MultiWriter(h, progress))
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
func (nopProgress) FileDone(int, bool)            {}
func (nopProgress) Done(bool)                     {}

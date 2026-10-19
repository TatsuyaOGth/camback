// Package plan decides what to do with every file on the card.
//
// Each file maps to <dest>/<card-relative path>. If something already exists
// there, it is compared in three stages (size, mtime ±2s, SHA-256). A media
// file that differs is a CONFLICT and aborts the whole run; any other file
// that differs is written next to it under a timestamped name (RENAME).
package plan

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"time"

	"github.com/TatsuyaOGth/camback/internal/hashing"
	"github.com/TatsuyaOGth/camback/internal/source"
	"github.com/TatsuyaOGth/camback/internal/transport"
)

// Action is the decision for one file.
type Action int

const (
	New      Action = iota // not on the destination: transfer
	Same                   // identical file already there: skip
	Rename                 // non-media file differs: transfer under a new name
	Conflict               // media file differs: abort
)

func (a Action) String() string {
	return [...]string{"NEW", "SAME", "RENAME", "CONFLICT"}[a]
}

// MtimeTolerance absorbs FAT's 2-second timestamp resolution.
const MtimeTolerance = 2 * time.Second

// Item is one planned file.
type Item struct {
	File     source.File
	Media    bool
	Action   Action
	Dest     transport.Path      // where the file goes (the new name for Rename)
	Existing *transport.FileInfo // what is at the original target, if anything
	Reason   string              // why it is Same/Conflict
}

// Plan is the result for one card.
type Plan struct {
	Dest  transport.Path
	Items []Item
}

// Count returns the number and total size of items with action a.
func (p *Plan) Count(a Action) (n int, bytes int64) {
	for _, it := range p.Items {
		if it.Action == a {
			n++
			bytes += it.File.Size
		}
	}
	return n, bytes
}

// Filter returns items whose action is one of as.
func (p *Plan) Filter(as ...Action) []Item {
	var out []Item
	for _, it := range p.Items {
		for _, a := range as {
			if it.Action == a {
				out = append(out, it)
				break
			}
		}
	}
	return out
}

// Options configures Build.
type Options struct {
	IsMedia func(name string) bool
	// RemoteHash hashes a destination file for stage 3. Defaults to
	// downloading it (hashing.ReadBack).
	RemoteHash func(ctx context.Context, p transport.Path) (string, error)
	// LocalHash hashes a card file. Defaults to hashing.File.
	LocalHash func(ctx context.Context, abs string) (string, error)
}

// Build compares every file against the destination. It only reads from the
// destination; nothing is written. It stops early when ctx is cancelled.
func Build(ctx context.Context, t transport.Transport, dest transport.Path, files []source.File, opt Options) (*Plan, error) {
	if opt.RemoteHash == nil {
		opt.RemoteHash = func(ctx context.Context, p transport.Path) (string, error) { return hashing.ReadBack(ctx, t, p) }
	}
	if opt.LocalHash == nil {
		opt.LocalHash = hashing.File
	}
	b := &builder{ctx: ctx, t: t, opt: opt, dirs: map[transport.Path]map[string]transport.FileInfo{}}

	p := &Plan{Dest: dest}
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		it, err := b.item(dest, f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Rel, err)
		}
		p.Items = append(p.Items, it)
	}
	return p, nil
}

type builder struct {
	ctx  context.Context
	t    transport.Transport
	opt  Options
	dirs map[transport.Path]map[string]transport.FileInfo // ReadDir cache
}

// lookup finds a name in a destination directory, listing each directory
// only once (one round trip per directory instead of one per file).
func (b *builder) lookup(p transport.Path) (*transport.FileInfo, error) {
	dir := p.Dir()
	entries, ok := b.dirs[dir]
	if !ok {
		list, err := b.t.ReadDir(dir)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		entries = make(map[string]transport.FileInfo, len(list))
		for _, fi := range list {
			entries[fi.Name] = fi
		}
		b.dirs[dir] = entries
	}
	if fi, ok := entries[p.Base()]; ok {
		return &fi, nil
	}
	return nil, nil
}

func (b *builder) item(dest transport.Path, f source.File) (Item, error) {
	it := Item{File: f, Media: b.opt.IsMedia(f.Rel), Dest: dest.Join(f.Rel)}
	existing, err := b.lookup(it.Dest)
	if err != nil {
		return it, err
	}
	if existing == nil {
		it.Action = New
		return it, nil
	}
	it.Existing = existing
	same, reason, err := b.same(f, it.Dest, *existing)
	if err != nil {
		return it, err
	}
	it.Reason = reason
	switch {
	case same:
		it.Action = Same
		return it, nil
	case it.Media:
		it.Action = Conflict
		return it, nil
	}

	// Non-media file that changed: keep both under a deterministic name.
	renamed := it.Dest.Dir().Join(RenamedName(it.Dest.Base(), f.ModTime))
	it.Dest = renamed
	prev, err := b.lookup(renamed)
	if err != nil {
		return it, err
	}
	if prev == nil {
		it.Action = Rename
		return it, nil
	}
	same, reason, err = b.same(f, renamed, *prev)
	if err != nil {
		return it, err
	}
	if same {
		it.Action = Same
		it.Reason = "already saved as " + renamed.Base()
		return it, nil
	}
	it.Action = Conflict
	it.Reason = "renamed target " + renamed.Base() + " also exists and differs (" + reason + ")"
	return it, nil
}

// same runs the three-stage comparison.
func (b *builder) same(f source.File, p transport.Path, r transport.FileInfo) (bool, string, error) {
	if r.IsDir {
		return false, "a directory exists at the destination", nil
	}
	if f.Size != r.Size {
		return false, fmt.Sprintf("size differs (card %d, destination %d bytes)", f.Size, r.Size), nil
	}
	if MtimeClose(f.ModTime, r.ModTime) {
		return true, "same size and modification time", nil
	}
	local, err := b.opt.LocalHash(b.ctx, f.Abs)
	if err != nil {
		return false, "", err
	}
	remote, err := b.opt.RemoteHash(b.ctx, p)
	if err != nil {
		return false, "", err
	}
	if local == remote {
		return true, "same content (SHA-256)", nil
	}
	return false, fmt.Sprintf("content differs (card mtime %s, destination %s)",
		f.ModTime.Format(time.DateTime), r.ModTime.Format(time.DateTime)), nil
}

// MtimeClose compares at second precision (SFTP v3 only carries seconds).
// time.Time values must be compared with Sub/Equal, never with ==.
func MtimeClose(a, b time.Time) bool {
	d := a.Truncate(time.Second).Sub(b.Truncate(time.Second))
	if d < 0 {
		d = -d
	}
	return d <= MtimeTolerance
}

// RenamedName inserts the file's modification time (local time) before the
// extension: MEDIAPRO.XML -> MEDIAPRO_20261016-092627.XML.
func RenamedName(base string, mtime time.Time) string {
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	if stem == "" { // e.g. ".hidden" has no stem; keep the whole name
		stem, ext = base, ""
	}
	return stem + "_" + mtime.Local().Format("20060102-150405") + ext
}

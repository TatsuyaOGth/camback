// Package camera decides which camera profile a card belongs to.
//
// A card is accepted only when every file that carries EXIF Make/Model
// matches the same profile. Mixed or unknown cameras abort the run.
package camera

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/TatsuyaOGth/camback/internal/config"
	"github.com/TatsuyaOGth/camback/internal/meta"
)

// Method tells how the camera was chosen.
type Method string

const (
	ByEXIF        Method = "exif"
	ByVolumeLabel Method = "volume_label"
	ByFlag        Method = "--camera"
)

// Result is a successful resolution.
type Result struct {
	Camera  *config.Camera
	Method  Method
	Matched int // files whose EXIF matched (ByEXIF only)
}

// MatchInfo returns the profile whose match list contains info, or nil.
func MatchInfo(cfg *config.Config, info meta.Info) *config.Camera {
	for i := range cfg.Cameras {
		for _, m := range cfg.Cameras[i].Match {
			if strings.TrimSpace(m.Make) == info.Make && strings.TrimSpace(m.Model) == info.Model {
				return &cfg.Cameras[i]
			}
		}
	}
	return nil
}

// MatchLabel returns the profiles whose volume_label is label, ignoring
// case (labels are often upper-cased by the OS).
func MatchLabel(cfg *config.Config, label string) []*config.Camera {
	var out []*config.Camera
	for i := range cfg.Cameras {
		if l := cfg.Cameras[i].VolumeLabel; l != "" && strings.EqualFold(l, label) {
			out = append(out, &cfg.Cameras[i])
		}
	}
	return out
}

// MixedError means EXIF matched more than one profile.
type MixedError struct {
	Counts   map[string]int    // camera id -> files
	Examples map[string]string // camera id -> example file
}

func (e *MixedError) Error() string {
	var b strings.Builder
	b.WriteString("files from more than one camera were found on this card:")
	for _, id := range sortedKeys(e.Counts) {
		fmt.Fprintf(&b, "\n  %-16s %5d files  (e.g. %s)", id, e.Counts[id], e.Examples[id])
	}
	return b.String()
}

// UnknownError means some EXIF Make/Model pairs match no profile.
type UnknownError struct {
	Counts   map[meta.Info]int
	Examples map[meta.Info]string
}

func (e *UnknownError) Error() string {
	keys := make([]meta.Info, 0, len(e.Counts))
	for k := range e.Counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i].Make+"\x00"+keys[i].Model < keys[j].Make+"\x00"+keys[j].Model
	})
	var b strings.Builder
	b.WriteString("files from a camera that is not in the config were found:")
	for _, k := range keys {
		fmt.Fprintf(&b, "\n  make = %q, model = %q  %d files  (e.g. %s)", k.Make, k.Model, e.Counts[k], e.Examples[k])
	}
	b.WriteString("\nadd a [[camera]] entry with this match to the config file")
	return b.String()
}

// ErrUndetermined means no file carried EXIF and no fallback applied.
var ErrUndetermined = errors.New("cannot determine the camera: no file has EXIF Make/Model and no volume_label matched; use --camera <id>")

// Resolve picks the camera for a card.
//   - infos: EXIF of the card's media files, keyed by card-relative path
//   - volumeLabel: label of the card volume
//   - flagID: value of --camera ("" if not given)
func Resolve(cfg *config.Config, infos map[string]meta.Info, volumeLabel, flagID string) (*Result, error) {
	counts := map[string]int{}
	examples := map[string]string{}
	unknown := &UnknownError{Counts: map[meta.Info]int{}, Examples: map[meta.Info]string{}}

	rels := make([]string, 0, len(infos))
	for rel := range infos {
		rels = append(rels, rel)
	}
	sort.Strings(rels) // deterministic examples
	for _, rel := range rels {
		info := infos[rel]
		if !info.Identified() {
			continue
		}
		cam := MatchInfo(cfg, info)
		if cam == nil {
			if unknown.Counts[info] == 0 {
				unknown.Examples[info] = rel
			}
			unknown.Counts[info]++
			continue
		}
		if counts[cam.ID] == 0 {
			examples[cam.ID] = rel
		}
		counts[cam.ID]++
	}

	var errs []error
	if len(unknown.Counts) > 0 {
		errs = append(errs, unknown)
	}
	if len(counts) > 1 {
		errs = append(errs, &MixedError{Counts: counts, Examples: examples})
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	if len(counts) == 1 {
		id := sortedKeys(counts)[0]
		if flagID != "" && flagID != id {
			return nil, fmt.Errorf("--camera %s contradicts EXIF, which identifies every file as %s", flagID, id)
		}
		cam, _ := cfg.Camera(id)
		return &Result{Camera: cam, Method: ByEXIF, Matched: counts[id]}, nil
	}

	// No EXIF at all: volume label, then --camera.
	if flagID != "" {
		cam, ok := cfg.Camera(flagID)
		if !ok {
			return nil, fmt.Errorf("--camera %s: no such camera in the config", flagID)
		}
		return &Result{Camera: cam, Method: ByFlag}, nil
	}
	byLabel := MatchLabel(cfg, volumeLabel)
	switch len(byLabel) {
	case 1:
		return &Result{Camera: byLabel[0], Method: ByVolumeLabel}, nil
	case 0:
		return nil, ErrUndetermined
	default:
		ids := make([]string, len(byLabel))
		for i, c := range byLabel {
			ids[i] = c.ID
		}
		return nil, fmt.Errorf("volume label %q matches several cameras (%s); use --camera <id>", volumeLabel, strings.Join(ids, ", "))
	}
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

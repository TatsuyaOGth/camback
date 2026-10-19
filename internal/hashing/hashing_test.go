package hashing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/TatsuyaOGth/camback/internal/transport"
	"github.com/TatsuyaOGth/camback/internal/transport/fake"
)

func sumOf(s string) string {
	b := sha256.Sum256([]byte(s))
	return hex.EncodeToString(b[:])
}

func newFS(t *testing.T) *fake.FS {
	t.Helper()
	fs := fake.New()
	fs.Put("/d/a", []byte("aaa"), time.Now())
	fs.Put("/d/b", []byte("bbb"), time.Now())
	fs.Put("/d/c", []byte("ccc"), time.Now())
	return fs
}

func TestRemoteHasherTrustsVerifiedSha256sum(t *testing.T) {
	fs := newFS(t)
	fs.HashSupported = true
	h := NewRemoteHasher(fs, func(s string) { t.Errorf("unexpected warning: %s", s) })
	for _, p := range []string{"/d/a", "/d/b", "/d/c"} {
		got, err := h.Sum(context.Background(), fakePath(p))
		if err != nil || got != sumOf(strings.Repeat(p[3:], 3)) {
			t.Fatalf("%s: %s, %v", p, got, err)
		}
	}
	// Only the first file is downloaded to check sha256sum.
	if fs.OpenCalls != 1 || fs.HashCalls != 3 {
		t.Errorf("open=%d hash=%d, want 1 and 3", fs.OpenCalls, fs.HashCalls)
	}
}

func TestRemoteHasherFallsBackOnWrongMapping(t *testing.T) {
	fs := newFS(t)
	fs.HashSupported = true
	fs.WrongHash = true
	var warnings []string
	h := NewRemoteHasher(fs, func(s string) { warnings = append(warnings, s) })
	for _, p := range []string{"/d/a", "/d/b"} {
		got, err := h.Sum(context.Background(), fakePath(p))
		if err != nil || got != sumOf(strings.Repeat(p[3:], 3)) {
			t.Fatalf("%s: %s, %v (a wrong sha256sum must never be used)", p, got, err)
		}
	}
	if len(warnings) != 1 || fs.OpenCalls != 2 {
		t.Errorf("warnings=%v open=%d", warnings, fs.OpenCalls)
	}
}

func TestRemoteHasherUnsupported(t *testing.T) {
	fs := newFS(t) // HashSupported = false
	h := NewRemoteHasher(fs, func(s string) { t.Errorf("no warning expected when unsupported: %s", s) })
	for _, p := range []string{"/d/a", "/d/b"} {
		if _, err := h.Sum(context.Background(), fakePath(p)); err != nil {
			t.Fatal(err)
		}
	}
	if fs.OpenCalls != 2 {
		t.Errorf("open=%d", fs.OpenCalls)
	}
}

func TestReaderCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Reader(ctx, strings.NewReader("data")); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}

func fakePath(s string) transport.Path { return transport.Path(s) }

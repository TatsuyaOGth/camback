package cli

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAwaitReturnsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	block := make(chan struct{})
	defer close(block)
	done := make(chan error, 1)
	go func() {
		_, err := await(ctx, func() (string, error) { <-block; return "late", nil })
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("await did not return after cancel")
	}
	// After cancellation, prompts return immediately without reading.
	if _, err := await(ctx, func() (int, error) { t.Error("read called after cancel"); return 0, nil }); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}

func TestAwaitValue(t *testing.T) {
	v, err := await(context.Background(), func() (string, error) { return "y", nil })
	if v != "y" || err != nil {
		t.Errorf("got %q, %v", v, err)
	}
}

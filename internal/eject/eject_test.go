package eject

import (
	"strings"
	"testing"
	"time"
)

func TestWaitGone(t *testing.T) {
	calls := 0
	goneAfter3 := func(string) bool { calls++; return calls < 3 }
	if err := waitGone(`E:\`, time.Second, time.Millisecond, goneAfter3); err != nil {
		t.Fatalf("err = %v", err)
	}

	stuck := func(string) bool { return true }
	err := waitGone(`E:\`, 20*time.Millisecond, time.Millisecond, stuck)
	if err == nil || !strings.Contains(err.Error(), "still mounted") {
		t.Fatalf("err = %v", err)
	}
}

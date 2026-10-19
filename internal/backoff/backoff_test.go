package backoff

import (
	"testing"
	"time"
)

func TestNextDoublesUpToTheMaximum(t *testing.T) {
	b := New(time.Second, 5*time.Second)
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second}
	for i, w := range want {
		if got := b.Next(); got != w {
			t.Errorf("Next() #%d = %v, want %v", i+1, got, w)
		}
	}
}

func TestResetStartsOver(t *testing.T) {
	b := New(time.Second, time.Minute)
	b.Next()
	b.Next()
	b.Reset()
	if got := b.Next(); got != time.Second {
		t.Errorf("Next() after Reset() = %v, want the base delay", got)
	}
}

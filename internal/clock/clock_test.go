package clock

import (
	"testing"
	"time"
)

func TestSystemClock(t *testing.T) {
	c := System()
	before := time.Now()
	now := c.Now()
	if now.Location() != time.UTC {
		t.Errorf("Now() location = %v, want UTC", now.Location())
	}
	if now.Before(before.Add(-time.Second)) || now.After(time.Now().Add(time.Second)) {
		t.Errorf("Now() = %v, want the current time", now)
	}
	select {
	case <-c.After(time.Millisecond):
	case <-time.After(5 * time.Second):
		t.Error("After() did not fire")
	}
}

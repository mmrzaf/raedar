package rules

import (
	"testing"
	"time"
)

func TestDatabaseWatch(t *testing.T) {
	start := time.Date(2026, 10, 18, 14, 0, 0, 0, time.UTC)
	var watch databaseWatch
	if outage, recovery := watch.observe(start, true); outage || recovery {
		t.Fatalf("observe() = %v, %v; want nothing before the threshold", outage, recovery)
	}
	if outage, _ := watch.observe(start.Add(90*time.Second), true); outage {
		t.Fatal("the outage was reported before two minutes")
	}
	outage, recovery := watch.observe(start.Add(2*time.Minute), true)
	if !outage || recovery {
		t.Fatalf("observe() = %v, %v; want the outage after two minutes", outage, recovery)
	}
	// It is reported once, not on every evaluation.
	if outage, _ := watch.observe(start.Add(3*time.Minute), true); outage {
		t.Fatal("the outage was reported twice")
	}
	outage, recovery = watch.observe(start.Add(4*time.Minute), false)
	if outage || !recovery {
		t.Fatalf("observe() = %v, %v; want a recovery", outage, recovery)
	}
	if _, recovery := watch.observe(start.Add(5*time.Minute), false); recovery {
		t.Fatal("the recovery was reported twice")
	}
}

func TestDatabaseWatchIgnoresShortFailures(t *testing.T) {
	start := time.Date(2026, 10, 18, 14, 0, 0, 0, time.UTC)
	var watch databaseWatch
	watch.observe(start, true)
	watch.observe(start.Add(time.Minute), true)
	if _, recovery := watch.observe(start.Add(90*time.Second), false); recovery {
		t.Fatal("a recovery was reported although the outage was never reported")
	}
	// A new outage starts from zero.
	if outage, _ := watch.observe(start.Add(2*time.Minute), true); outage {
		t.Fatal("the new outage was reported immediately")
	}
	if outage, _ := watch.observe(start.Add(4*time.Minute), true); !outage {
		t.Fatal("the new outage was not reported after two minutes")
	}
}

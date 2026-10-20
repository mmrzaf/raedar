package notify

import (
	"testing"
	"time"
)

func TestGateInQuietWindow(t *testing.T) {
	g := &Gate{QuietZone: time.UTC}
	from := time.Date(0, 1, 1, 22, 0, 0, 0, time.UTC)
	to := time.Date(0, 1, 1, 7, 0, 0, 0, time.UTC)
	g.QuietFrom = &from
	g.QuietTo = &to

	cases := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"before window (06:00)", time.Date(2026, 1, 1, 6, 0, 0, 0, time.UTC), true},
		{"inside window (23:00)", time.Date(2026, 1, 1, 23, 0, 0, 0, time.UTC), true},
		{"after window start (22:00)", time.Date(2026, 1, 1, 22, 0, 0, 0, time.UTC), true},
		{"after window end (07:00)", time.Date(2026, 1, 1, 7, 0, 0, 0, time.UTC), false},
		{"daytime (12:00)", time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := g.inQuietWindow(tc.at); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGateInQuietWindowSameDay(t *testing.T) {
	zone := time.UTC
	g := &Gate{QuietZone: zone}
	from := time.Date(0, 1, 1, 9, 0, 0, 0, zone)
	to := time.Date(0, 1, 1, 17, 0, 0, 0, zone)
	g.QuietFrom = &from
	g.QuietTo = &to

	if g.inQuietWindow(time.Date(2026, 1, 1, 12, 0, 0, 0, zone)) != true {
		t.Fatalf("12:00 should be inside window")
	}
	if g.inQuietWindow(time.Date(2026, 1, 1, 19, 0, 0, 0, zone)) != false {
		t.Fatalf("19:00 should be outside")
	}
}

func TestGateInQuietWindowDisabled(t *testing.T) {
	g := &Gate{QuietZone: time.UTC}
	if g.inQuietWindow(time.Now()) {
		t.Fatalf("nil window should never be inside")
	}
}

package main

import (
	"testing"
	"time"
)

func TestNextRollupTime(t *testing.T) {
	tests := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{
			name: "at the hour boundary",
			now:  time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC),
			want: time.Date(2026, 10, 18, 10, 5, 0, 0, time.UTC),
		},
		{
			name: "before the rollup",
			now:  time.Date(2026, 10, 18, 10, 3, 0, 0, time.UTC),
			want: time.Date(2026, 10, 18, 10, 5, 0, 0, time.UTC),
		},
		{
			name: "at the rollup",
			now:  time.Date(2026, 10, 18, 10, 5, 0, 0, time.UTC),
			want: time.Date(2026, 10, 18, 11, 5, 0, 0, time.UTC),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nextRollupTime(tt.now); !got.Equal(tt.want) {
				t.Errorf("nextRollupTime(%v) = %v, want %v", tt.now, got, tt.want)
			}
		})
	}
}

func TestNextDaily(t *testing.T) {
	location := time.FixedZone("UTC+3:30", 3*3600+30*60)
	tests := []struct {
		name string
		now  time.Time
		hour int
		want time.Time
	}{
		{
			name: "before the hour",
			now:  time.Date(2026, 10, 18, 1, 0, 0, 0, location),
			hour: 3,
			want: time.Date(2026, 10, 18, 3, 0, 0, 0, location),
		},
		{
			name: "after the hour",
			now:  time.Date(2026, 10, 18, 4, 0, 0, 0, location),
			hour: 3,
			want: time.Date(2026, 10, 19, 3, 0, 0, 0, location),
		},
		{
			name: "exactly at the hour",
			now:  time.Date(2026, 10, 18, 3, 0, 0, 0, location),
			hour: 3,
			want: time.Date(2026, 10, 19, 3, 0, 0, 0, location),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nextTimeOfDay(tt.now, location, tt.hour, 0); !got.Equal(tt.want) {
				t.Errorf("nextTimeOfDay(%v) = %v, want %v", tt.now, got, tt.want)
			}
		})
	}
	// A UTC time crossing the local midnight lands on the right local day.
	now := time.Date(2026, 10, 18, 22, 0, 0, 0, time.UTC) // 01:30 local on the 19th
	want := time.Date(2026, 10, 19, 3, 0, 0, 0, location)
	if got := nextTimeOfDay(now, location, 3, 0); !got.Equal(want) {
		t.Errorf("nextTimeOfDay(%v) = %v, want %v", now, got, want)
	}
}

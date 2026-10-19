package store

import (
	"reflect"
	"testing"
)

func TestHistogramAdd(t *testing.T) {
	tests := []struct {
		name         string
		milliseconds float64
		wantBucket   int
	}{
		{name: "below the first bound", milliseconds: 0, wantBucket: 0},
		{name: "negative", milliseconds: -5, wantBucket: 0},
		{name: "at the first bound", milliseconds: 5, wantBucket: 0},
		{name: "just above the first bound", milliseconds: 5.1, wantBucket: 1},
		{name: "at a middle bound", milliseconds: 100, wantBucket: 5},
		{name: "above a middle bound", milliseconds: 100.1, wantBucket: 6},
		{name: "at the last finite bound", milliseconds: 60000, wantBucket: 19},
		{name: "above the last bound", milliseconds: 60000.1, wantBucket: 20},
		{name: "far above the last bound", milliseconds: 125000, wantBucket: 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var histogram Histogram
			histogram.Add(tt.milliseconds)
			if histogram.Count() != 1 {
				t.Fatalf("count = %d, want 1", histogram.Count())
			}
			if histogram[tt.wantBucket] != 1 {
				t.Errorf("histogram = %v, want one count in bucket %d", histogram, tt.wantBucket)
			}
		})
	}
}

func TestHistogramMerge(t *testing.T) {
	var first Histogram
	first.Add(3)
	first.Add(120)
	var second Histogram
	second.Add(3)
	second.Add(40000)
	first.Merge(&second)
	want := Histogram{}
	want[0] = 2
	want[6] = 1
	want[19] = 1
	if first != want {
		t.Errorf("merged = %v, want %v", first, want)
	}
	if first.Count() != 4 {
		t.Errorf("count = %d, want 4", first.Count())
	}
}

func TestHistogramPercentile(t *testing.T) {
	tests := []struct {
		name    string
		values  []float64
		percent float64
		want    float64
		over    bool
	}{
		{name: "empty", percent: 95, want: 0},
		{
			name:    "single bucket",
			values:  repeatValue(30, 100),
			percent: 95,
			want:    48.75,
		},
		{
			name:    "exactly at a bucket edge",
			values:  append(repeatValue(100, 50), repeatValue(150, 50)...),
			percent: 50,
			want:    100,
		},
		{
			name:    "first percentile",
			values:  []float64{10, 20, 30},
			percent: 0,
			want:    5,
		},
		{
			name:    "median in the first bucket",
			values:  []float64{1, 2, 3},
			percent: 50,
			want:    2.5,
		},
		{
			name:    "open-ended bucket",
			values:  []float64{90000, 120000},
			percent: 95,
			want:    HistogramBound,
			over:    true,
		},
		{
			name:    "open-ended bucket at the maximum",
			values:  []float64{60100},
			percent: 100,
			want:    HistogramBound,
			over:    true,
		},
		{
			name:    "above one hundred is clamped",
			values:  []float64{1, 2, 3, 4, 1000},
			percent: 150,
			want:    1000,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var histogram Histogram
			for _, value := range tt.values {
				histogram.Add(value)
			}
			got, over := histogram.Percentile(tt.percent)
			if got != tt.want || over != tt.over {
				t.Errorf("Percentile(%v) = %v, %v, want %v, %v", tt.percent, got, over, tt.want, tt.over)
			}
		})
	}
}

func TestHistogramCounts(t *testing.T) {
	var histogram Histogram
	histogram[0] = 5
	histogram[4] = 1
	histogram[20] = 2
	want := make([]int32, Buckets)
	want[0], want[4], want[20] = 5, 1, 2
	if got := histogram.Counts(); !reflect.DeepEqual(got, want) {
		t.Errorf("Counts() = %v, want %v", got, want)
	}
}

// repeatValue returns n observations of one value.
func repeatValue(value float64, n int) []float64 {
	values := make([]float64, n)
	for i := range values {
		values[i] = value
	}
	return values
}

// Package aggregate turns access log measurements into per-minute and
// per-path buckets and writes them to the database.
package store

import (
	"fmt"
	"math"
)

// Buckets is the number of latency histogram buckets.
const Buckets = 21

// HistogramBound is where a percentile falls in the open-ended last bucket;
// such values are shown as "over 60 s".
const HistogramBound = 60000

// bucketBounds returns the upper bounds of the first Buckets-1 latency
// buckets, in milliseconds; the last bucket is open-ended.
func bucketBounds() []float64 {
	return []float64{
		5, 10, 25, 50, 75, 100, 150, 200, 300, 400,
		500, 750, 1000, 1500, 2000, 3000, 5000, 10000, 30000, 60000,
	}
}

// Histogram counts observations per latency bucket. Counts are kept as int64
// so that histograms can be merged freely; the database stores them as int4.
type Histogram [Buckets]int64

// Add records one observation in milliseconds. Negative values count in the
// first bucket.
func (h *Histogram) Add(milliseconds float64) {
	bounds := bucketBounds()
	for i, upper := range bounds {
		if milliseconds <= upper {
			h[i]++
			return
		}
	}
	h[Buckets-1]++
}

// Merge adds another histogram to this one.
func (h *Histogram) Merge(other *Histogram) {
	for i := range h {
		h[i] += other[i]
	}
}

// Count returns the number of observations.
func (h *Histogram) Count() int64 {
	var total int64
	for _, count := range h {
		total += count
	}
	return total
}

// Percentile returns the interpolated percentile in milliseconds and whether
// it falls in the open-ended last bucket. Values in that bucket are returned
// as HistogramBound and are displayed as "over 60 s". p is clamped to 0-100.
func (h *Histogram) Percentile(p float64) (float64, bool) {
	total := h.Count()
	if total == 0 {
		return 0, false
	}
	p = math.Min(math.Max(p, 0), 100)
	target := p / 100 * float64(total)
	bounds := bucketBounds()
	var before int64
	for i := range h {
		if h[i] == 0 {
			continue
		}
		after := before + h[i]
		if float64(after) < target {
			before = after
			continue
		}
		if i >= len(bounds) {
			return HistogramBound, true
		}
		upper := bounds[i]
		lower := 0.0
		if i > 0 {
			lower = bounds[i-1]
		}
		position := 1.0
		if h[i] > 0 {
			position = (target - float64(before)) / float64(h[i])
		}
		position = math.Min(math.Max(position, 0), 1)
		return lower + (upper-lower)*position, false
	}
	return HistogramBound, true
}

// Counts returns the histogram as the int4 array the database stores,
// saturating at the largest int4.
func (h *Histogram) Counts() []int32 {
	counts := make([]int32, Buckets)
	for i, count := range h {
		if count > math.MaxInt32 {
			count = math.MaxInt32
		}
		counts[i] = int32(count)
	}
	return counts
}

// LoadHistogram copies counts read from the database into a histogram.
func LoadHistogram(counts []int32) (Histogram, error) {
	var histogram Histogram
	if len(counts) != Buckets {
		return histogram, fmt.Errorf("histogram has %d buckets, want %d", len(counts), Buckets)
	}
	for i, count := range counts {
		if count < 0 {
			return histogram, fmt.Errorf("histogram bucket %d is negative", i)
		}
		histogram[i] = int64(count)
	}
	return histogram, nil
}

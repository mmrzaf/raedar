// Package backoff spaces out the attempts of something that keeps failing: the
// delay starts small, doubles with every failure and stops growing at a
// maximum.
package backoff

import "time"

// Backoff is the delay schedule of one retrying loop. It is not safe for
// concurrent use.
type Backoff struct {
	base, max time.Duration
	delay     time.Duration
}

// New returns a schedule that starts at base and doubles up to max.
func New(base, max time.Duration) *Backoff {
	return &Backoff{base: base, max: max}
}

// Next returns how long to wait before the next attempt after a failure.
func (b *Backoff) Next() time.Duration {
	if b.delay == 0 {
		b.delay = b.base
	} else {
		b.delay = min(b.delay*2, b.max)
	}
	return b.delay
}

// Reset starts the schedule over; call it after a success.
func (b *Backoff) Reset() {
	b.delay = 0
}

// Package clock is the time source of the components that wait or timestamp.
// They take a Clock instead of calling the time package, so tests can run
// minutes of behavior without waiting.
package clock

import "time"

// Clock provides the time.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// After returns a channel that receives the current time after d.
	After(d time.Duration) <-chan time.Time
}

// System returns the clock of the running process.
func System() Clock {
	return system{}
}

// system is the clock of the running process.
type system struct{}

// Now returns the current time in UTC.
func (system) Now() time.Time {
	return time.Now().UTC()
}

// After returns a channel that receives the current time after d.
func (system) After(d time.Duration) <-chan time.Time {
	return time.After(d)
}

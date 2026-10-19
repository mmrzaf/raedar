package store

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/mmrzaf/raedar/internal/backoff"
)

// retryBase and retryMax bound the delay between connection attempts.
const (
	retryBase = time.Second
	retryMax  = 30 * time.Second
)

// sleeper provides timers to the store, so that tests run without waiting.
type sleeper interface {
	// After returns a channel that receives the current time after d.
	After(d time.Duration) <-chan time.Time
}

// fatalError wraps an error that retrying cannot fix.
type fatalError struct {
	err error
}

// Error returns the message of the wrapped error.
func (e fatalError) Error() string {
	return e.err.Error()
}

// Unwrap returns the wrapped error.
func (e fatalError) Unwrap() error {
	return e.err
}

// retry calls fn until it succeeds, waiting longer between every attempt up
// to max, and stops when ctx is done. It returns the error of the last
// attempt, or the error inside a fatalError immediately.
func retry(ctx context.Context, c sleeper, base, max time.Duration, logger *slog.Logger, fn func(context.Context) error) error {
	delays := backoff.New(base, max)
	for attempt := 1; ; attempt++ {
		err := fn(ctx)
		if err == nil {
			return nil
		}
		var fatal fatalError
		if errors.As(err, &fatal) {
			return err
		}
		if ctx.Err() != nil {
			return err
		}
		delay := delays.Next()
		logger.Warn("retrying", slog.Int("attempt", attempt), slog.Duration("wait", delay), slog.String("error", err.Error()))
		select {
		case <-ctx.Done():
			return err
		case <-c.After(delay):
		}
	}
}

package store

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// fakeClock records the wait durations and fires immediately.
type fakeClock struct {
	waits []time.Duration
}

// After records d and returns a channel that is ready at once.
func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.waits = append(c.waits, d)
	ch := make(chan time.Time, 1)
	ch <- time.Unix(0, 0)
	return ch
}

// cancellingClock cancels a context when it is asked to wait.
type cancellingClock struct {
	cancel context.CancelFunc
	waits  int
}

// After cancels the context and returns a channel that never fires.
func (c *cancellingClock) After(time.Duration) <-chan time.Time {
	c.waits++
	c.cancel()
	return make(chan time.Time)
}

// discardLogger returns a logger that writes nothing.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestRetrySucceedsAfterFailures(t *testing.T) {
	c := &fakeClock{}
	attempts := 0
	err := retry(context.Background(), c, time.Second, 8*time.Second, discardLogger(), func(context.Context) error {
		attempts++
		if attempts < 3 {
			return errors.New("not yet")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("retry() error = %v", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
	want := []time.Duration{time.Second, 2 * time.Second}
	if !reflect.DeepEqual(c.waits, want) {
		t.Errorf("waits = %v, want %v", c.waits, want)
	}
}

func TestRetryCapsTheDelay(t *testing.T) {
	c := &fakeClock{}
	attempts := 0
	err := retry(context.Background(), c, time.Second, 4*time.Second, discardLogger(), func(context.Context) error {
		attempts++
		if attempts < 6 {
			return errors.New("not yet")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("retry() error = %v", err)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second, 4 * time.Second}
	if !reflect.DeepEqual(c.waits, want) {
		t.Errorf("waits = %v, want %v", c.waits, want)
	}
}

func TestRetryStopsOnFatalError(t *testing.T) {
	c := &fakeClock{}
	unfixable := errors.New("invalid password")
	attempts := 0
	err := retry(context.Background(), c, time.Second, time.Second, discardLogger(), func(context.Context) error {
		attempts++
		return fatalError{err: unfixable}
	})
	if !errors.Is(err, unfixable) {
		t.Errorf("retry() error = %v, want %v", err, unfixable)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	if len(c.waits) != 0 {
		t.Errorf("waits = %v, want none", c.waits)
	}
}

func TestRetryStopsWhenContextIsDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &fakeClock{}
	attempts := 0
	err := retry(ctx, c, time.Second, time.Second, discardLogger(), func(context.Context) error {
		attempts++
		return errors.New("unreachable")
	})
	if err == nil {
		t.Fatal("retry() error = nil, want error")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	if len(c.waits) != 0 {
		t.Errorf("waits = %v, want none", c.waits)
	}
}

func TestRetryStopsWhenContextIsCancelledWhileWaiting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &cancellingClock{cancel: cancel}
	attempts := 0
	err := retry(ctx, c, time.Second, time.Second, discardLogger(), func(context.Context) error {
		attempts++
		return errors.New("unreachable")
	})
	if !errors.Is(err, context.Canceled) && err.Error() != "unreachable" {
		t.Errorf("retry() error = %v, want the attempt error or context.Canceled", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	if c.waits != 1 {
		t.Errorf("waits = %d, want 1", c.waits)
	}
}

func TestUnrecoverable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "auth failure", err: &pgconn.PgError{Code: "28P01"}, want: true},
		{name: "missing database", err: &pgconn.PgError{Code: "3D000"}, want: true},
		{name: "connection refused", err: errors.New("connection refused"), want: false},
		{name: "too many connections", err: &pgconn.PgError{Code: "53300"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := unrecoverable(tt.err); got != tt.want {
				t.Errorf("unrecoverable(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestLoadMigrations(t *testing.T) {
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations() error = %v", err)
	}
	if len(migrations) == 0 {
		t.Fatal("loadMigrations() = no migrations, want the embedded ones")
	}
	if migrations[0].version != 1 {
		t.Errorf("first migration version = %d, want 1", migrations[0].version)
	}
	for i, m := range migrations {
		if strings.TrimSpace(m.sql) == "" {
			t.Errorf("migration %s is empty", m.name)
		}
		if i > 0 && migrations[i-1].version >= m.version {
			t.Errorf("migration %s comes after %s", m.name, migrations[i-1].name)
		}
	}
}

func TestOpenInvalidDatabaseURLDoesNotLeakPassword(t *testing.T) {
	_, err := Open(context.Background(), "postgres://raedar:supersecret@example.com:notaport/raedar", discardLogger())
	if err == nil {
		t.Fatal("Open() error = nil, want a parse error")
	}
	if strings.Contains(err.Error(), "supersecret") {
		t.Errorf("Open() error = %q, want the password redacted", err.Error())
	}
	if !strings.Contains(err.Error(), "parse database URL") {
		t.Errorf("Open() error = %q, want it to mention the database URL", err.Error())
	}
}

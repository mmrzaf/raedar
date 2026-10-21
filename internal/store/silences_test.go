//go:build integration

package store

import (
	"testing"
	"time"

	"github.com/mmrzaf/raedar/internal/testdb"
)

func silenceStr(s string) *string { return &s }

// silenceStore opens a store, runs migrations, and returns it.
func silenceStore(t *testing.T, prefix string) (*Store, string) {
	t.Helper()
	databaseURL, schema := testdb.NewSchema(t, prefix)
	databaseURL = testdb.WithSearchPath(t, databaseURL, schema)
	ctx := t.Context()
	s, err := Open(ctx, databaseURL, nil)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(s.Close)
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s, prefix
}

func mustSilence(t *testing.T, ctx timeoutContext, s *Store, silence Silence) {
	t.Helper()
	if _, err := s.CreateSilence(ctx, silence); err != nil {
		t.Fatalf("create silence: %v", err)
	}
}

func mustMatch(t *testing.T, ctx timeoutContext, s *Store, rule, subject string, now time.Time) *Silence {
	t.Helper()
	got, err := s.FindActiveSilence(ctx, rule, subject, now)
	if err != nil {
		t.Fatalf("find silence: %v", err)
	}
	return got
}

// timeoutContext matches context.Context via a small interface to keep tests
// flexible across Go versions.
type timeoutContext interface {
	Deadline() (time.Time, bool)
	Done() <-chan struct{}
	Err() error
	Value(any) any
}

func TestSilencePrecedenceIntegration(t *testing.T) {
	s, _ := silenceStore(t, "silences_prec")
	ctx := t.Context()
	now := time.Now()

	mustSilence(t, ctx, s, Silence{
		Kind:     SilenceOneShotMute,
		Rule:     silenceStr("route.down"),
		Subject:  silenceStr("fallback@file"),
		Reason:   "exact",
		StartsAt: now.Add(-time.Minute), EndsAt: now.Add(time.Hour),
	})
	mustSilence(t, ctx, s, Silence{
		Kind: SilenceMaintenanceWindow, Subject: silenceStr("fallback@file"),
		Reason: "subject-only", StartsAt: now.Add(-time.Minute), EndsAt: now.Add(time.Hour),
	})
	mustSilence(t, ctx, s, Silence{
		Kind:     SilenceAutoDeploy,
		Rule:     silenceStr("route.down"),
		Reason:   "rule-only",
		StartsAt: now.Add(-time.Minute), EndsAt: now.Add(time.Hour),
	})
	mustSilence(t, ctx, s, Silence{
		Kind:     SilenceQuietHours,
		Reason:   "global",
		StartsAt: now.Add(-time.Minute), EndsAt: now.Add(time.Hour),
	})

	// Rule + subject → exact
	got := mustMatch(t, ctx, s, "route.down", "fallback@file", now)
	if got.Reason != "exact" {
		t.Fatalf("want exact, got %q", got.Reason)
	}
	// Rule + different subject → rule-only
	got = mustMatch(t, ctx, s, "route.down", "other", now)
	if got.Reason != "rule-only" {
		t.Fatalf("want rule-only, got %q", got.Reason)
	}
	// Different rule + subject match → subject-only
	got = mustMatch(t, ctx, s, "host.disk", "fallback@file", now)
	if got.Reason != "subject-only" {
		t.Fatalf("want subject-only, got %q", got.Reason)
	}
	// Neither matches → global
	got = mustMatch(t, ctx, s, "host.disk", "/", now)
	if got.Reason != "global" {
		t.Fatalf("want global, got %q", got.Reason)
	}
}

func TestSilenceActiveWindowIntegration(t *testing.T) {
	s, _ := silenceStore(t, "silences_window")
	ctx := t.Context()
	now := time.Now()

	mustSilence(t, ctx, s, Silence{
		Kind:     SilenceOneShotMute,
		Rule:     silenceStr("r"),
		Subject:  silenceStr("s"),
		Reason:   "future",
		StartsAt: now.Add(time.Hour), EndsAt: now.Add(2 * time.Hour),
	})
	got := mustMatch(t, ctx, s, "r", "s", now)
	if got != nil {
		t.Fatalf("want nil for not-yet-active silence, got %+v", got)
	}
}

func TestSilenceDedupIntegration(t *testing.T) {
	s, _ := silenceStore(t, "silences_dedup")
	ctx := t.Context()
	now := time.Now()

	key := "deploy:foo:v1"
	id1, err := s.CreateSilence(ctx, Silence{
		Kind: SilenceAutoDeploy, DedupKey: &key,
		Reason: "first", StartsAt: now, EndsAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	id2, err := s.CreateSilence(ctx, Silence{
		Kind: SilenceAutoDeploy, DedupKey: &key,
		Reason: "second", StartsAt: now, EndsAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("dedup did not collapse: id1=%d id2=%d", id1, id2)
	}
}

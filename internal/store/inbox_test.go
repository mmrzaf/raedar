//go:build integration

package store

import (
	"testing"
	"time"

	"github.com/mmrzaf/raedar/internal/testdb"
)

func TestClaimUpdateIntegration(t *testing.T) {
	databaseURL, schema := testdb.NewSchema(t, "inbox_claim")
	databaseURL = testdb.WithSearchPath(t, databaseURL, schema)
	ctx := t.Context()
	st, err := Open(ctx, databaseURL, nil)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	claimed, err := st.ClaimUpdate(ctx, 42)
	if err != nil {
		t.Fatalf("ClaimUpdate first: %v", err)
	}
	if !claimed {
		t.Fatal("first claim must own the update")
	}
	claimed, err = st.ClaimUpdate(ctx, 42)
	if err != nil {
		t.Fatalf("ClaimUpdate second: %v", err)
	}
	if claimed {
		t.Fatal("second claim must not own the update")
	}
}

func TestPruneProcessedUpdatesIntegration(t *testing.T) {
	databaseURL, schema := testdb.NewSchema(t, "inbox_prune")
	databaseURL = testdb.WithSearchPath(t, databaseURL, schema)
	ctx := t.Context()
	st, err := Open(ctx, databaseURL, nil)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if _, err := st.ClaimUpdate(ctx, 1); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// Backdate the row so the prune has something to remove.
	if _, err := st.pool.Exec(ctx,
		`update telegram_inbox set processed_at = now() - interval '72 hours' where update_id = 1`); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	removed, err := st.PruneProcessedUpdates(ctx, 48*time.Hour)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
}

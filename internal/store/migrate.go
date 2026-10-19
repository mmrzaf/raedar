package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// createMigrationsTable creates the table that records applied migrations.
const createMigrationsTable = `create table if not exists schema_migrations (
	version integer primary key
)`

// migration is one numbered SQL file.
type migration struct {
	version int
	name    string
	sql     string
}

// Migrate applies pending migrations in order, each in its own transaction,
// under the migration lock that serializes instances.
func (s *Store) Migrate(ctx context.Context) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, createMigrationsTable); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	if _, err := conn.Exec(ctx, "select pg_advisory_lock($1)", migrationLockID); err != nil {
		return fmt.Errorf("take migration lock: %w", err)
	}
	defer s.releaseMigrationLock(ctx, conn)
	applied, err := appliedVersions(ctx, conn)
	if err != nil {
		return err
	}
	latest := migrations[len(migrations)-1].version
	for version := range applied {
		if version > latest {
			return fmt.Errorf("database schema version %d is newer than this Raedar version", version)
		}
	}
	count := 0
	for _, m := range migrations {
		if applied[m.version] {
			continue
		}
		if err := s.applyMigration(ctx, m); err != nil {
			return err
		}
		count++
	}
	if count > 0 {
		s.logger.Info("migrations applied", slog.Int("count", count))
	}
	return nil
}

// applyMigration runs one migration and records its version in the same
// transaction.
func (s *Store) applyMigration(ctx context.Context, m migration) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migration %s: %w", m.name, err)
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			s.logger.Warn("roll back migration", slog.String("name", m.name), slog.String("error", err.Error()))
		}
	}()
	if _, err := tx.Exec(ctx, m.sql); err != nil {
		return fmt.Errorf("apply migration %s: %w", m.name, err)
	}
	if _, err := tx.Exec(ctx, "insert into schema_migrations (version) values ($1)", m.version); err != nil {
		return fmt.Errorf("record migration %s: %w", m.name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migration %s: %w", m.name, err)
	}
	return nil
}

// releaseMigrationLock gives the migration lock back. The lock is released
// with the connection anyway, so a failure here is not fatal.
func (s *Store) releaseMigrationLock(ctx context.Context, conn *pgxpool.Conn) {
	if _, err := conn.Exec(context.WithoutCancel(ctx), "select pg_advisory_unlock($1)", migrationLockID); err != nil {
		s.logger.Debug("release migration lock", slog.String("error", err.Error()))
	}
}

// appliedVersions returns the migration versions already recorded.
func appliedVersions(ctx context.Context, conn *pgxpool.Conn) (map[int]bool, error) {
	rows, err := conn.Query(ctx, "select version from schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}
	defer rows.Close()
	applied := make(map[int]bool)
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("scan applied migration: %w", err)
		}
		applied[version] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}
	return applied, nil
}

// loadMigrations reads the embedded migration files in version order.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	migrations := make([]migration, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		prefix, _, ok := strings.Cut(name, "_")
		if !ok || !strings.HasSuffix(name, ".sql") {
			return nil, fmt.Errorf("migration file %s: want a name like 0001_initial.sql", name)
		}
		version, err := strconv.Atoi(prefix)
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("migration file %s: version must be a positive number", name)
		}
		content, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", name, err)
		}
		migrations = append(migrations, migration{version: version, name: name, sql: string(content)})
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].version < migrations[j].version })
	for i := 1; i < len(migrations); i++ {
		if migrations[i].version == migrations[i-1].version {
			return nil, fmt.Errorf("migration version %d appears twice", migrations[i].version)
		}
	}
	return migrations, nil
}

// SchemaVersion returns the highest applied migration version, and 0 when the
// database has not been migrated.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var version *int
	if err := s.pool.QueryRow(ctx, `select max(version) from schema_migrations`).Scan(&version); err != nil {
		return 0, fmt.Errorf("read the schema version: %w", err)
	}
	if version == nil {
		return 0, nil
	}
	return *version, nil
}

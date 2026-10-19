package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// The advisory lock keys identify Raedar's locks on a database: one instance
// holds the instance lock while it runs, and migrations are applied under the
// migration lock.
const (
	instanceLockID  = 0x7261656461720001
	migrationLockID = 0x7261656461720002
)

// lockReleaseTimeout bounds the attempt to give the instance lock back.
const lockReleaseTimeout = 5 * time.Second

// Lock takes the instance lock on this database. It returns ErrLockTaken when
// another instance already holds it.
func (s *Store) Lock(ctx context.Context) error {
	if s.lockConn != nil {
		return errors.New("instance lock already held")
	}
	return s.acquireLock(ctx)
}

// KeepLock verifies that the instance lock is still held. When the lock
// connection was lost, KeepLock takes the lock again; a lock taken by another
// instance in the meantime is reported as ErrLockTaken.
func (s *Store) KeepLock(ctx context.Context) error {
	if s.lockConn == nil {
		return errors.New("instance lock not held")
	}
	if err := s.lockConn.Conn().Ping(ctx); err == nil {
		return nil
	}
	s.logger.Warn("instance lock connection lost, taking the lock again")
	s.lockConn.Release()
	s.lockConn = nil
	return s.acquireLock(ctx)
}

// acquireLock takes the instance advisory lock on a dedicated connection and
// keeps that connection until the lock is released.
func (s *Store) acquireLock(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection for the instance lock: %w", err)
	}
	var taken bool
	if err := conn.QueryRow(ctx, "select pg_try_advisory_lock($1)", instanceLockID).Scan(&taken); err != nil {
		conn.Release()
		return fmt.Errorf("take instance lock: %w", err)
	}
	if !taken {
		conn.Release()
		return ErrLockTaken
	}
	s.lockConn = conn
	return nil
}

// releaseLock gives the instance lock back and returns its connection to the
// pool.
func (s *Store) releaseLock() {
	if s.lockConn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), lockReleaseTimeout)
	defer cancel()
	if _, err := s.lockConn.Exec(ctx, "select pg_advisory_unlock($1)", instanceLockID); err != nil {
		s.logger.Debug("release instance lock", slog.String("error", err.Error()))
	}
	s.lockConn.Release()
	s.lockConn = nil
}

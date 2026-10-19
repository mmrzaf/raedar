//go:build integration

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// testDatabaseURL returns the disposable test database URL.
func testDatabaseURL(t *testing.T) string {
	t.Helper()
	databaseURL := os.Getenv("RAEDAR_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("RAEDAR_TEST_DATABASE_URL is not set")
	}
	return databaseURL
}

// childEnv returns the environment without RAEDAR_ variables, plus extra.
func childEnv(extra map[string]string) []string {
	env := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "RAEDAR_") {
			continue
		}
		env = append(env, entry)
	}
	for name, value := range extra {
		env = append(env, name+"="+value)
	}
	return env
}

// withSearchPath returns the URL with search_path set to schema.
func withSearchPath(t *testing.T, databaseURL, schema string) string {
	t.Helper()
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", databaseURL, err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// createSchema creates one schema for a test.
func createSchema(t *testing.T, databaseURL, schema string) {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect to PostgreSQL: %v", err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "create schema "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}
}

// dropSchema removes a schema and everything in it.
func dropSchema(t *testing.T, databaseURL, schema string) {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Errorf("connect to PostgreSQL: %v", err)
		return
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "drop schema "+pgx.Identifier{schema}.Sanitize()+" cascade"); err != nil {
		t.Errorf("drop schema %s: %v", schema, err)
	}
}

// buildBinary builds the raedar binary for the tests in this file.
func buildBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "raedar")
	cmd := exec.Command("go", "build", "-o", path, ".")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build raedar: %v", err)
	}
	return path
}

// testMutexID serializes the tests that use the database-wide instance lock.
// It must match the value used by the store integration tests.
const testMutexID = 0x726165646172ffff

// lockInstanceTests waits until no other test process uses the instance lock
// and releases the mutex when the test ends.
func lockInstanceTests(t *testing.T, databaseURL string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect to PostgreSQL: %v", err)
	}
	if _, err := conn.Exec(ctx, "select pg_advisory_lock($1)", testMutexID); err != nil {
		conn.Close(ctx)
		t.Fatalf("lock the test mutex: %v", err)
	}
	t.Cleanup(func() {
		defer conn.Close(ctx)
		if _, err := conn.Exec(ctx, "select pg_advisory_unlock($1)", testMutexID); err != nil {
			t.Errorf("unlock the test mutex: %v", err)
		}
	})
}

func TestServeRunsHoldsTheLockAndStopsOnSignal(t *testing.T) {
	databaseURL := testDatabaseURL(t)
	lockInstanceTests(t, databaseURL)
	binary := buildBinary(t)
	schema := fmt.Sprintf("raedar_serve_%d", time.Now().UnixNano())
	createSchema(t, databaseURL, schema)
	t.Cleanup(func() { dropSchema(t, databaseURL, schema) })
	env := childEnv(map[string]string{
		"RAEDAR_DATABASE_URL": withSearchPath(t, databaseURL, schema),
		"RAEDAR_HOST_NAME":    "test-host",
	})

	first := exec.Command(binary)
	first.Env = env
	stdout, err := first.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	var firstErr bytes.Buffer
	first.Stderr = &firstErr
	if err := first.Start(); err != nil {
		t.Fatalf("start first instance: %v", err)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- first.Wait() }()
	var (
		outputMu sync.Mutex
		output   bytes.Buffer
	)
	captured := func() string {
		outputMu.Lock()
		defer outputMu.Unlock()
		return output.String()
	}
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			outputMu.Lock()
			output.WriteString(scanner.Text())
			output.WriteByte('\n')
			outputMu.Unlock()
		}
	}()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(30 * time.Second)
	for !strings.Contains(captured(), `"msg":"running"`) {
		select {
		case err := <-stopped:
			t.Fatalf("first instance exited before running: %v (stderr: %s)\n%s", err, firstErr.String(), captured())
		case <-ticker.C:
		case <-deadline:
			first.Process.Kill()
			t.Fatalf("first instance did not start (stderr: %s)\n%s", firstErr.String(), captured())
		}
	}

	secondCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	second := exec.CommandContext(secondCtx, binary)
	second.Env = env
	var secondOutput bytes.Buffer
	second.Stdout = &secondOutput
	second.Stderr = &secondOutput
	err = second.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("second instance error = %v, want exit code 1 (output: %s)", err, secondOutput.String())
	}
	if exitErr.ExitCode() != 1 {
		t.Errorf("second instance exit code = %d, want 1 (output: %s)", exitErr.ExitCode(), secondOutput.String())
	}
	if !strings.Contains(secondOutput.String(), "another instance holds the lock") {
		t.Errorf("second instance output = %q, want it to mention the lock", secondOutput.String())
	}

	if err := first.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}
	select {
	case err := <-stopped:
		if err != nil {
			t.Errorf("first instance exit = %v, want 0 (stderr: %s)", err, firstErr.String())
		}
	case <-time.After(30 * time.Second):
		first.Process.Kill()
		t.Fatal("first instance did not stop after SIGTERM")
	}
}

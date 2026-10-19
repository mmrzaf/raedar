package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"version"}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit code = %d, want %d", code, exitOK)
	}
	if got, want := stdout.String(), "raedar dev\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

func TestRunUsageErrors(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantMessage string
	}{
		{name: "unknown command", args: []string{"frobnicate"}, wantMessage: `unknown command "frobnicate"`},
		{name: "version with arguments", args: []string{"version", "extra"}, wantMessage: "version takes no arguments"},
		{name: "serve with arguments", args: []string{"serve", "extra"}, wantMessage: "serve takes no arguments"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), tt.args, &stdout, &stderr)
			if code != exitUsage {
				t.Errorf("exit code = %d, want %d", code, exitUsage)
			}
			if !strings.Contains(stderr.String(), tt.wantMessage) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantMessage)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty", stdout.String())
			}
		})
	}
}

func TestRunWithoutCommandValidatesConfiguration(t *testing.T) {
	t.Setenv("RAEDAR_LISTEN", "not-an-address")
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), nil, &stdout, &stderr)
	if code != exitError {
		t.Errorf("exit code = %d, want %d", code, exitError)
	}
	if !strings.Contains(stderr.String(), "RAEDAR_LISTEN") {
		t.Errorf("stderr = %q, want it to name RAEDAR_LISTEN", stderr.String())
	}
}

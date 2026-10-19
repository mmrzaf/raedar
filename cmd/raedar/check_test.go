package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHealthAddress(t *testing.T) {
	tests := []struct {
		name   string
		listen string
		want   string
	}{
		{name: "wildcard IPv4", listen: "0.0.0.0:8080", want: "127.0.0.1:8080"},
		{name: "wildcard IPv6", listen: "[::]:8080", want: "127.0.0.1:8080"},
		{name: "empty host", listen: ":8080", want: "127.0.0.1:8080"},
		{name: "loopback", listen: "127.0.0.1:9090", want: "127.0.0.1:9090"},
		{name: "private address", listen: "10.0.0.5:8080", want: "10.0.0.5:8080"},
		{name: "without a port", listen: "localhost", want: "localhost"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := healthAddress(tt.listen); got != tt.want {
				t.Errorf("healthAddress(%q) = %q, want %q", tt.listen, got, tt.want)
			}
		})
	}
}

func TestHealthCheck(t *testing.T) {
	t.Run("healthy", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/healthz" {
				t.Errorf("path = %q, want /healthz", r.URL.Path)
			}
			w.Write([]byte("ok\n"))
		}))
		defer server.Close()
		if err := healthCheck(context.Background(), strings.TrimPrefix(server.URL, "http://"), server.Client()); err != nil {
			t.Fatalf("healthCheck() error = %v", err)
		}
	})
	t.Run("unhealthy", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		}))
		defer server.Close()
		err := healthCheck(context.Background(), strings.TrimPrefix(server.URL, "http://"), server.Client())
		if err == nil || !strings.Contains(err.Error(), "503") {
			t.Fatalf("healthCheck() error = %v, want the 503", err)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		client := &http.Client{Timeout: time.Second}
		if err := healthCheck(context.Background(), "127.0.0.1:1", client); err == nil {
			t.Fatal("healthCheck() error = nil, want a connection error")
		}
	})
}

func TestRunCheckInvalidConfiguration(t *testing.T) {
	t.Setenv("RAEDAR_DATABASE_URL", "mysql://raedar:raedar@mysql:3306/raedar")
	var stdout, stderr bytes.Buffer
	if code := runCheck(context.Background(), nil, &stdout, &stderr); code != exitError {
		t.Fatalf("runCheck() = %d, want %d", code, exitError)
	}
	if !strings.Contains(stderr.String(), "invalid configuration") {
		t.Errorf("stderr = %q, want the configuration error", stderr.String())
	}
	if strings.Contains(stdout.String(), "postgresql") {
		t.Error("a check ran although the configuration is invalid")
	}
}

func TestRunCheckRejectsArguments(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runCheck(context.Background(), []string{"extra"}, &stdout, &stderr); code != exitUsage {
		t.Errorf("runCheck() = %d, want %d", code, exitUsage)
	}
	if code := runHealthcheck(context.Background(), []string{"extra"}, &stdout, &stderr); code != exitUsage {
		t.Errorf("runHealthcheck() = %d, want %d", code, exitUsage)
	}
}

func TestRunHealthcheckUnreachable(t *testing.T) {
	t.Setenv("RAEDAR_DATABASE_URL", "postgres://raedar:raedar@postgres:5432/raedar?sslmode=disable")
	t.Setenv("RAEDAR_LISTEN", "127.0.0.1:1")
	var stdout, stderr bytes.Buffer
	if code := runHealthcheck(context.Background(), nil, &stdout, &stderr); code != exitError {
		t.Fatalf("runHealthcheck() = %d, want %d", code, exitError)
	}
	if !strings.Contains(stderr.String(), "healthcheck") {
		t.Errorf("stderr = %q, want the healthcheck error", stderr.String())
	}
}

func TestRunHealthcheckSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	}))
	defer server.Close()
	t.Setenv("RAEDAR_DATABASE_URL", "postgres://raedar:raedar@postgres:5432/raedar?sslmode=disable")
	t.Setenv("RAEDAR_LISTEN", strings.TrimPrefix(server.URL, "http://"))
	var stdout, stderr bytes.Buffer
	if code := runHealthcheck(context.Background(), nil, &stdout, &stderr); code != exitOK {
		t.Fatalf("runHealthcheck() = %d, want %d (stderr %q)", code, exitOK, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "ok" {
		t.Errorf("stdout = %q, want ok", stdout.String())
	}
}

func TestParseChannel(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		want        string
		wantProblem bool
	}{
		{name: "all channels", want: ""},
		{name: "telegram", args: []string{"--channel", "telegram"}, want: "telegram"},
		{name: "equals form", args: []string{"--channel=sms"}, want: "sms"},
		{name: "webhook", args: []string{"--channel=webhook"}, want: "webhook"},
		{name: "unknown channel", args: []string{"--channel=pager"}, wantProblem: true},
		{name: "missing value", args: []string{"--channel"}, wantProblem: true},
		{name: "unknown argument", args: []string{"--verbose"}, wantProblem: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			channel, problem := parseChannel(tt.args)
			if tt.wantProblem {
				if problem == "" {
					t.Fatal("parseChannel() problem = \"\", want an error")
				}
				return
			}
			if problem != "" {
				t.Fatalf("parseChannel() problem = %q", problem)
			}
			if channel != tt.want {
				t.Errorf("parseChannel() = %q, want %q", channel, tt.want)
			}
		})
	}
}

package memlimit

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

// writeCgroup writes one cgroup file and returns the cgroup root.
func writeCgroup(t *testing.T, path, content string) string {
	t.Helper()
	root := t.TempDir()
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
	return root
}

// assertErrorContains asserts that err is not nil and mentions want.
func assertErrorContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want error containing %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want it to contain %q", err.Error(), want)
	}
}

func TestApply(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		content string
		want    int64
		wantErr string
	}{
		{name: "cgroup v2", path: "memory.max", content: "1073741824\n", want: 858993459},
		{name: "cgroup v1", path: "memory/memory.limit_in_bytes", content: "2147483648\n", want: 1717986918},
		{name: "v2 unlimited", path: "memory.max", content: "max\n", wantErr: "no memory limit"},
		{name: "v1 unlimited", path: "memory/memory.limit_in_bytes", content: "9223372036854771712\n", wantErr: "no memory limit"},
		{name: "zero", path: "memory.max", content: "0\n", wantErr: "no memory limit"},
		{name: "malformed", path: "memory.max", content: "lots\n", wantErr: "invalid cgroup memory limit"},
		{name: "empty", path: "memory.max", content: "", wantErr: "invalid cgroup memory limit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := writeCgroup(t, tt.path, tt.content)
			got, err := Apply(root)
			if tt.wantErr != "" {
				assertErrorContains(t, err, tt.wantErr)
				if got != 0 {
					t.Errorf("Apply() = %d, want 0", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Apply() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("Apply() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestApplyWithoutCgroupFiles(t *testing.T) {
	got, err := Apply(t.TempDir())
	assertErrorContains(t, err, "no cgroup memory limit")
	if got != 0 {
		t.Errorf("Apply() = %d, want 0", got)
	}
}

func TestApplyRespectsGOMEMLIMIT(t *testing.T) {
	t.Setenv("GOMEMLIMIT", "64MiB")
	root := writeCgroup(t, "memory.max", "1073741824\n")
	got, err := Apply(root)
	assertErrorContains(t, err, "GOMEMLIMIT is set")
	if got != 0 {
		t.Errorf("Apply() = %d, want 0", got)
	}
}

func TestApplySetsTheRuntimeLimit(t *testing.T) {
	previous := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(previous) })
	root := writeCgroup(t, "memory.max", "1073741824\n")
	got, err := Apply(root)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if runtimeLimit := debug.SetMemoryLimit(-1); runtimeLimit != got {
		t.Errorf("runtime memory limit = %d, want %d", runtimeLimit, got)
	}
}

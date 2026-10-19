// Package memlimit sets the process memory limit from the container's memory
// limit.
package memlimit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
)

// share is the fraction of the container's memory limit used for the Go
// memory limit, in tenths.
const share = 8

// unlimited is the limit at and above which the container counts as
// unlimited; the kernel reports such values for containers without a limit.
const unlimited = 1 << 59

// Apply sets the process memory limit to 80 % of the container's memory limit
// and returns the limit in bytes. It returns 0 and an error when nothing was
// applied: GOMEMLIMIT is set, or the container has no memory limit.
func Apply(cgroupRoot string) (int64, error) {
	if os.Getenv("GOMEMLIMIT") != "" {
		return 0, errors.New("GOMEMLIMIT is set")
	}
	limit, err := containerLimit(cgroupRoot)
	if err != nil {
		return 0, err
	}
	applied := limit * share / 10
	debug.SetMemoryLimit(applied)
	return applied, nil
}

// containerLimit reads the cgroup v2 or v1 memory limit in bytes.
func containerLimit(cgroupRoot string) (int64, error) {
	v2 := filepath.Join(cgroupRoot, "memory.max")
	if data, err := os.ReadFile(v2); err == nil {
		return parseLimit(string(data))
	}
	v1 := filepath.Join(cgroupRoot, "memory", "memory.limit_in_bytes")
	data, err := os.ReadFile(v1)
	if err != nil {
		return 0, fmt.Errorf("no cgroup memory limit under %s", cgroupRoot)
	}
	return parseLimit(string(data))
}

// parseLimit reads a memory limit. "max" and values at or above 2^59 mean the
// container has no limit.
func parseLimit(data string) (int64, error) {
	text := strings.TrimSpace(data)
	if text == "max" {
		return 0, errors.New("the container has no memory limit")
	}
	limit, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid cgroup memory limit %q", text)
	}
	if limit <= 0 || limit >= unlimited {
		return 0, errors.New("the container has no memory limit")
	}
	return limit, nil
}

// Package systemd reads unit state from systemd over the system D-Bus.
package systemd

import (
	"context"
	"strings"
	"time"
)

// Reader reads unit state from systemd.
type Reader interface {
	// ListUnits returns the state of every unit.
	ListUnits(ctx context.Context) ([]Unit, error)
}

// Unit is one unit as systemd reports it.
type Unit struct {
	// Name is the unit name.
	Name string
	// ActiveState and SubState are systemd's states.
	ActiveState, SubState string
	// Result is the result of the unit's last run, empty when it has none.
	// For a timer it is the result of the unit the timer activates.
	Result string
	// ExitStatus is the exit status of the last run, nil when there is none.
	// For a timer it is the exit status of the unit the timer activates.
	ExitStatus *int
	// Since is when the unit entered its current state, nil when unknown.
	Since *time.Time
}

// Matches reports whether a unit name matches one of the patterns, where *
// matches any sequence of characters.
func Matches(patterns []string, name string) bool {
	for _, pattern := range patterns {
		if matchPattern(pattern, name) {
			return true
		}
	}
	return false
}

// matchPattern matches one pattern with * wildcards.
func matchPattern(pattern, name string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return name == parts[0]
	}
	if !strings.HasPrefix(name, parts[0]) {
		return false
	}
	name = name[len(parts[0]):]
	last := parts[len(parts)-1]
	if !strings.HasSuffix(name, last) {
		return false
	}
	name = name[:len(name)-len(last)]
	for _, part := range parts[1 : len(parts)-1] {
		index := strings.Index(name, part)
		if index < 0 {
			return false
		}
		name = name[index+len(part):]
	}
	return true
}

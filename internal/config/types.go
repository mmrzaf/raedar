package config

import (
	"fmt"
	"strconv"
	"time"
)

// EnvLookup reads one environment variable. os.LookupEnv implements it.
type EnvLookup func(name string) (value string, ok bool)

// Percent is a percentage between 0 and 100 as written in the configuration,
// for example 85, 85% or 2.5%.
type Percent float64

// ParsePercent parses a percentage as written in the configuration, for
// example 85, 85% or 2.5%.
func ParsePercent(s string) (Percent, error) {
	return parsePercent(s)
}

// ParseDuration parses a duration as written in the configuration, for
// example 300ms, 90s, 5m, 6h or 14d.
func ParseDuration(s string) (time.Duration, error) {
	return parseDuration(s)
}

// String returns the percentage with a percent sign, for example "85%" or
// "2.5%".
func (p Percent) String() string {
	return strconv.FormatFloat(float64(p), 'f', -1, 64) + "%"
}

// TimeOfDay is a time of day in the configured time zone, without a date.
type TimeOfDay struct {
	// Hour is the hour, 0 to 23.
	Hour int
	// Minute is the minute, 0 to 59.
	Minute int
}

// String returns the time in HH:MM form.
func (t TimeOfDay) String() string {
	return fmt.Sprintf("%02d:%02d", t.Hour, t.Minute)
}

// SystemdMode selects how Raedar treats the systemd D-Bus socket.
type SystemdMode string

const (
	// SystemdAuto reads systemd when the D-Bus socket exists.
	SystemdAuto SystemdMode = "auto"
	// SystemdOn makes a missing D-Bus socket an error.
	SystemdOn SystemdMode = "on"
	// SystemdOff disables the systemd reader.
	SystemdOff SystemdMode = "off"
)

// Disk is one filesystem the host collector watches.
type Disk struct {
	// Name is the display name: the path relative to the host root, or the
	// name given as name=path.
	Name string
	// Path is the path inside the container.
	Path string
}

// Header is one HTTP header.
type Header struct {
	// Name is the header name.
	Name string
	// Value is the header value.
	Value string
}

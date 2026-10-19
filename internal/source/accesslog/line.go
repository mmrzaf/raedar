// Package accesslog reads the Traefik access log, in file mode or from the
// Traefik container's output, and feeds measurements to the aggregator.
package accesslog

import (
	"encoding/json"
	"strings"
	"time"
)

// line is one access log line as Traefik writes it.
type line struct {
	Time                  time.Time `json:"time"`
	StartUTC              time.Time `json:"StartUTC"`
	Duration              float64   `json:"Duration"`
	RouterName            string    `json:"RouterName"`
	ServiceName           string    `json:"ServiceName"`
	RequestHost           string    `json:"RequestHost"`
	RequestMethod         string    `json:"RequestMethod"`
	RequestPath           string    `json:"RequestPath"`
	DownstreamStatus      int       `json:"DownstreamStatus"`
	OriginStatus          int       `json:"OriginStatus"`
	DownstreamContentSize int64     `json:"DownstreamContentSize"`
}

// Line is one parsed access log entry.
type Line struct {
	// Time is when Traefik wrote the line, which is when the request
	// completed.
	Time time.Time
	// RouterName is the Traefik router name, for example shop@docker; empty
	// when no router matched.
	RouterName string
	// ServiceName is the Traefik service name.
	ServiceName string
	// Host is the request host.
	Host string
	// Method is the request method.
	Method string
	// Path is the request path.
	Path string
	// Status is the downstream status code.
	Status int
	// OriginStatus is the status the backend answered with, 0 when Traefik
	// answered itself.
	OriginStatus int
	// Duration is the request duration.
	Duration time.Duration
	// Bytes is the downstream content size.
	Bytes int64
}

// parseLine parses one access log line. It reports false for lines that are
// not access log entries, such as Traefik's own log output.
func parseLine(data []byte) (Line, bool) {
	var entry line
	if err := json.Unmarshal(data, &entry); err != nil {
		return Line{}, false
	}
	if entry.DownstreamStatus < 100 {
		return Line{}, false
	}
	when := entry.Time
	if when.IsZero() {
		when = entry.StartUTC.Add(time.Duration(entry.Duration))
	}
	return Line{
		Time:         when,
		RouterName:   entry.RouterName,
		ServiceName:  entry.ServiceName,
		Host:         entry.RequestHost,
		Method:       entry.RequestMethod,
		Path:         entry.RequestPath,
		Status:       entry.DownstreamStatus,
		OriginStatus: entry.OriginStatus,
		Duration:     time.Duration(entry.Duration),
		Bytes:        entry.DownstreamContentSize,
	}, true
}

// noBackend reports whether Traefik answered because it could not reach any
// backend.
func (l Line) noBackend() bool {
	switch l.Status {
	case 502, 503, 504:
		return l.OriginStatus == 0
	}
	return false
}

// countLatency reports whether the request duration belongs in the latency
// histogram: protocol upgrades and ignored paths do not.
func (l Line) countLatency(ignore []string) bool {
	if l.Status == 101 {
		return false
	}
	for _, prefix := range ignore {
		if strings.HasPrefix(l.Path, prefix) {
			return false
		}
	}
	return true
}

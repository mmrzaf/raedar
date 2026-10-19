package web

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/mmrzaf/raedar/internal/docker"
)

// streamLines writes container log lines as server-sent events.
func (s *Server) streamLines(w io.Writer, logs io.Reader, until time.Time) {
	flusher, _ := w.(interface{ Flush() })
	scanner := bufio.NewScanner(logs)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !until.IsZero() {
			if stamp, _, found := strings.Cut(line, " "); found {
				if at, err := time.Parse(time.RFC3339Nano, stamp); err == nil && at.After(until) {
					return
				}
			}
		}
		data, err := json.Marshal(map[string]string{"line": line})
		if err != nil {
			continue
		}
		if _, err := w.Write([]byte("data: " + string(data) + "\n\n")); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// trimLines keeps the log lines that end before until.
func trimLines(body []byte, until time.Time, location *time.Location) []byte {
	if until.IsZero() {
		return body
	}
	var kept bytes.Buffer
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		if stamp, _, found := strings.Cut(line, " "); found {
			if at, err := time.Parse(time.RFC3339Nano, stamp); err == nil && at.After(until) {
				break
			}
		}
		kept.WriteString(line)
		kept.WriteByte('\n')
	}
	_ = location
	return kept.Bytes()
}

// dockerLogOptions builds the log request options.
func dockerLogOptions(since time.Time, follow, tty bool) docker.LogOptions {
	return docker.LogOptions{Follow: follow, Timestamps: true, Since: since, TTY: tty}
}

// ioReadAll reads the whole log body.
func ioReadAll(reader io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(reader, 8<<20))
}

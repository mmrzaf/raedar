package web

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mmrzaf/raedar/internal/docker"
)

// maxLogBytes caps the log text returned for a window, so one chatty
// container cannot exhaust memory.
const maxLogBytes = 8 << 20

// streamLines writes container log lines as server-sent events until the
// stream ends or the client disconnects.
func (s *Server) streamLines(w io.Writer, logs io.Reader) {
	flusher, _ := w.(interface{ Flush() })
	scanner := bufio.NewScanner(logs)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		data, err := json.Marshal(map[string]string{"line": scanner.Text()})
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

// trimLines keeps the log lines that are stamped at or before until.
func trimLines(body []byte, until time.Time) []byte {
	if until.IsZero() {
		return body
	}
	var kept bytes.Buffer
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
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
	return kept.Bytes()
}

// dockerLogOptions builds the log request options.
func dockerLogOptions(since time.Time, follow, tty bool) docker.LogOptions {
	return docker.LogOptions{Follow: follow, Timestamps: true, Since: since, TTY: tty}
}

// logsAPI returns container logs, streamed as server-sent events when follow is set.
func (s *Server) logsAPI(w http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	container := query.Get("container")
	if container == "" {
		badRequest(w, "container is required")
		return
	}
	since, okSince := parseTime(query.Get("since"))
	until, okUntil := parseTime(query.Get("until"))
	if !okSince {
		badRequest(w, "since is required")
		return
	}
	if !okUntil {
		until = since.Add(4 * time.Minute)
	}
	follow := query.Get("follow") == "true"
	s.streamLogs(w, request, container, since, until, follow)
}

// streamLogs sends one container's logs: the lines from since to until as
// plain text, or, when follow is set, every line from since on as server-sent
// events until the client goes away.
func (s *Server) streamLogs(w http.ResponseWriter, request *http.Request, container string, since, until time.Time, follow bool) {
	client, err := s.connector.Client(request.Context())
	if err != nil {
		s.logger.Warn("container logs unavailable", "error", err)
		http.Error(w, "the Docker API is not reachable", http.StatusServiceUnavailable)
		return
	}
	tty := false
	if model := s.model.Model(); model != nil {
		for _, running := range model.Containers {
			if running.ID == container || running.Name == container {
				tty = running.TTY
				container = running.ID
				break
			}
		}
	}
	logs, err := client.Logs(request.Context(), container, dockerLogOptions(since, follow, tty))
	if err != nil {
		s.logger.Warn("container logs unavailable", "error", err)
		http.Error(w, "cannot read the container logs", http.StatusBadGateway)
		return
	}
	defer logs.Close()
	if follow {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming is not supported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		s.streamLines(w, logs)
		return
	}
	body, err := io.ReadAll(io.LimitReader(logs, maxLogBytes))
	if err != nil {
		http.Error(w, "cannot read the container logs", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if _, err := w.Write(trimLines(body, until)); err != nil {
		s.logger.Debug("write container logs", "error", err)
	}
}

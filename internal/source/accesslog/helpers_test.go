package accesslog

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mmrzaf/raedar/internal/discovery"
)

// discoveryContainerWithID returns a Traefik container stand-in.
func discoveryContainerWithID(id string) *discovery.Container {
	return &discovery.Container{ID: id, Name: "traefik", TTY: true}
}

// logsServer is a Docker API stand-in that streams one container's log.
type logsServer struct {
	server *httptest.Server
}

// newLogsServer starts a stand-in that streams the lines with Docker
// timestamps.
func newLogsServer(t *testing.T, lines []string) *logsServer {
	t.Helper()
	holder := &logsServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Version":"27.1.1","ApiVersion":"1.46"}`))
	})
	mux.HandleFunc("/v1.46/containers/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/logs") {
			http.NotFound(w, r)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming is not supported", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		for i, line := range lines {
			stamp := testBase.Add(time.Duration(i) * time.Millisecond).Format(time.RFC3339Nano)
			fmt.Fprintf(w, "%s %s\n", stamp, line)
			flusher.Flush()
		}
		<-r.Context().Done()
	})
	holder.server = httptest.NewServer(mux)
	t.Cleanup(holder.server.Close)
	return holder
}

// host returns the address to pass to the source.
func (s *logsServer) host() string {
	return "tcp://" + strings.TrimPrefix(s.server.URL, "http://")
}

// positionAtStart returns a saved position that makes a follower read a file
// from its beginning.
func positionAtStart(t *testing.T, path string) []byte {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	data, err := json.Marshal(filePosition{Inode: inodeOf(info), Offset: 0})
	if err != nil {
		t.Fatalf("marshal position: %v", err)
	}
	return data
}

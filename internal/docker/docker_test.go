package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// containerID is the container the recorded responses are about.
const containerID = "1f8a4c2d9e7b6a5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b7c6d5e4f3a2b1c"

// testServer is a Docker API stand-in that records the requests it receives.
type testServer struct {
	server   *httptest.Server
	mu       sync.Mutex
	requests []string
}

// newTestServer starts a server running handle and recording request URIs.
func newTestServer(t *testing.T, handle http.HandlerFunc) *testServer {
	t.Helper()
	recorder := &testServer{}
	recorder.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder.mu.Lock()
		recorder.requests = append(recorder.requests, r.URL.RequestURI())
		recorder.mu.Unlock()
		handle(w, r)
	}))
	t.Cleanup(recorder.server.Close)
	return recorder
}

// host returns the address to pass to Connect.
func (s *testServer) host() string {
	return "tcp://" + strings.TrimPrefix(s.server.URL, "http://")
}

// uris returns the request URIs seen so far.
func (s *testServer) uris() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

// testdata reads one recorded response.
func testdata(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read testdata/%s: %v", name, err)
	}
	return data
}

// connect starts a client against a server that replays recorded responses.
func connect(t *testing.T, handle http.HandlerFunc) (*Client, *testServer) {
	t.Helper()
	server := newTestServer(t, handle)
	client, err := Connect(context.Background(), server.host())
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	return client, server
}

// replay serves the recorded responses for the versioned endpoints.
func replay(t *testing.T) http.HandlerFunc {
	files := map[string]string{
		"/v1.46/info":            "info.json",
		"/v1.46/containers/json": "containers.json",
		"/v1.46/containers/" + containerID + "/json":  "inspect.json",
		"/v1.46/containers/" + containerID + "/stats": "stats.json",
		"/v1.46/system/df":                            "df.json",
	}
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/_ping":
			w.Write([]byte("OK"))
		case r.URL.Path == "/version":
			w.Write(testdata(t, "version.json"))
		case files[r.URL.Path] != "":
			w.Write(testdata(t, files[r.URL.Path]))
		default:
			http.NotFound(w, r)
		}
	}
}

func TestConnectNegotiatesAPIVersion(t *testing.T) {
	client, server := connect(t, replay(t))
	if client.apiVersion != "1.46" {
		t.Errorf("apiVersion = %q, want 1.46", client.apiVersion)
	}
	uris := server.uris()
	if len(uris) != 1 || uris[0] != "/version" {
		t.Errorf("requests = %v, want only /version", uris)
	}
}

func TestPing(t *testing.T) {
	client, server := connect(t, replay(t))
	if err := client.Ping(context.Background()); err != nil {
		t.Fatalf("Ping() error = %v", err)
	}
	uris := server.uris()
	if uris[len(uris)-1] != "/_ping" {
		t.Errorf("last request = %q, want /_ping", uris[len(uris)-1])
	}
}

func TestInfo(t *testing.T) {
	client, _ := connect(t, replay(t))
	info, err := client.Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if info.NCPU != 8 {
		t.Errorf("NCPU = %d, want 8", info.NCPU)
	}
	if info.MemTotal != 16789889024 {
		t.Errorf("MemTotal = %d, want 16789889024", info.MemTotal)
	}
}

func TestContainers(t *testing.T) {
	client, server := connect(t, replay(t))
	containers, err := client.Containers(context.Background())
	if err != nil {
		t.Fatalf("Containers() error = %v", err)
	}
	if len(containers) != 3 {
		t.Fatalf("containers = %d, want 3", len(containers))
	}
	first := containers[0]
	if first.ID != containerID {
		t.Errorf("ID = %q, want %q", first.ID, containerID)
	}
	if first.Name != "shop-api-1" {
		t.Errorf("Name = %q, want shop-api-1", first.Name)
	}
	if first.ImageRef != "shop/api:2.4.0" {
		t.Errorf("ImageRef = %q, want shop/api:2.4.0", first.ImageRef)
	}
	if first.ImageID != "sha256:1111111111111111111111111111111111111111111111111111111111111111" {
		t.Errorf("ImageID = %q, want the recorded image ID", first.ImageID)
	}
	if first.State != "running" {
		t.Errorf("State = %q, want running", first.State)
	}
	if want := time.Unix(1725000000, 0).UTC(); !first.Created.Equal(want) {
		t.Errorf("Created = %v, want %v", first.Created, want)
	}
	if first.Labels["com.docker.compose.project"] != "shop" {
		t.Errorf("Labels[com.docker.compose.project] = %q, want shop", first.Labels["com.docker.compose.project"])
	}
	last := server.uris()[len(server.uris())-1]
	if last != "/v1.46/containers/json?all=1" {
		t.Errorf("last request = %q, want the versioned container list", last)
	}
}

func TestInspect(t *testing.T) {
	client, _ := connect(t, replay(t))
	inspect, err := client.Inspect(context.Background(), containerID)
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if inspect.Name != "shop-api-1" {
		t.Errorf("Name = %q, want shop-api-1", inspect.Name)
	}
	if inspect.ImageRef != "shop/api:2.4.0" {
		t.Errorf("ImageRef = %q, want shop/api:2.4.0", inspect.ImageRef)
	}
	if inspect.State != "running" || inspect.Health != "healthy" {
		t.Errorf("State, Health = %q, %q, want running, healthy", inspect.State, inspect.Health)
	}
	if inspect.RestartPolicy != "unless-stopped" {
		t.Errorf("RestartPolicy = %q, want unless-stopped", inspect.RestartPolicy)
	}
	if want := time.Date(2024, 8, 30, 9, 20, 0, 123456789, time.UTC); !inspect.Created.Equal(want) {
		t.Errorf("Created = %v, want %v", inspect.Created, want)
	}
	if want := time.Date(2024, 8, 30, 9, 20, 1, 987654321, time.UTC); !inspect.StartedAt.Equal(want) {
		t.Errorf("StartedAt = %v, want %v", inspect.StartedAt, want)
	}
	if inspect.TTY {
		t.Error("TTY = true, want false")
	}
	wantNetworks := []Network{{Name: "atlas_web", IPAddress: "172.20.0.4"}}
	if !reflect.DeepEqual(inspect.Networks, wantNetworks) {
		t.Errorf("Networks = %+v, want %+v", inspect.Networks, wantNetworks)
	}
	if _, ok := inspect.Ports["8080/tcp"]; !ok {
		t.Errorf("Ports = %v, want an entry for 8080/tcp", inspect.Ports)
	}
	if inspect.Labels["traefik.http.routers.shop-api.rule"] != "Host(`shop.example.com`)" {
		t.Errorf("Labels = %v, want the Traefik router label", inspect.Labels)
	}
}

func TestStats(t *testing.T) {
	client, _ := connect(t, replay(t))
	stats, err := client.Stats(context.Background(), containerID)
	if err != nil {
		t.Fatalf("Stats() error = %v", err)
	}
	if stats.CPUTotalUsage != 12345678900 {
		t.Errorf("CPUTotalUsage = %d, want 12345678900", stats.CPUTotalUsage)
	}
	if stats.MemoryUsage != 188743680 {
		t.Errorf("MemoryUsage = %d, want 188743680", stats.MemoryUsage)
	}
	if stats.MemoryLimit != 536870912 {
		t.Errorf("MemoryLimit = %d, want 536870912", stats.MemoryLimit)
	}
	if stats.MemoryStats["inactive_file"] != 41943040 {
		t.Errorf("MemoryStats[inactive_file] = %d, want 41943040", stats.MemoryStats["inactive_file"])
	}
	if stats.NetworkRxBytes != 1048576+512 {
		t.Errorf("NetworkRxBytes = %d, want %d", stats.NetworkRxBytes, 1048576+512)
	}
	if stats.NetworkTxBytes != 2097152+256 {
		t.Errorf("NetworkTxBytes = %d, want %d", stats.NetworkTxBytes, 2097152+256)
	}
	if want := time.Date(2024, 8, 30, 11, 30, 0, 1, time.UTC); !stats.Read.Equal(want) {
		t.Errorf("Read = %v, want %v", stats.Read, want)
	}
}

func TestDiskUsage(t *testing.T) {
	client, _ := connect(t, replay(t))
	usage, err := client.DiskUsage(context.Background())
	if err != nil {
		t.Fatalf("DiskUsage() error = %v", err)
	}
	want := DiskUsage{
		ImagesBytes:            524288000 + 419430400 + 209715200 + 157286400 + 104857600,
		ImagesReclaimableBytes: 157286400 + 104857600,
		BuildCacheBytes:        314572800 + 104857600,
		VolumesBytes:           1073741824 + 536870912 + 262144000,
		ContainersBytes:        2048000 + 1048576,
	}
	if usage != want {
		t.Errorf("DiskUsage() = %+v, want %+v", usage, want)
	}
}

// frame builds one Docker stream frame.
func frame(stream byte, payload string) []byte {
	header := make([]byte, frameHeader)
	header[0] = stream
	binary.BigEndian.PutUint32(header[4:], uint32(len(payload)))
	return append(header, payload...)
}

func TestLogsRemovesFraming(t *testing.T) {
	body := append(frame(1, "2024-08-30T09:20:01.000000001Z GET /orders 200\n"), frame(2, "2024-08-30T09:20:02.000000001Z an error\n")...)
	client, server := connect(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			w.Write(testdata(t, "version.json"))
			return
		}
		w.Write(body)
	})
	logs, err := client.Logs(context.Background(), containerID, LogOptions{Timestamps: true})
	if err != nil {
		t.Fatalf("Logs() error = %v", err)
	}
	defer logs.Close()
	got, err := io.ReadAll(logs)
	if err != nil {
		t.Fatalf("read logs: %v", err)
	}
	want := "2024-08-30T09:20:01.000000001Z GET /orders 200\n2024-08-30T09:20:02.000000001Z an error\n"
	if string(got) != want {
		t.Errorf("logs = %q, want %q", got, want)
	}
	last := server.uris()[len(server.uris())-1]
	if last != "/v1.46/containers/"+containerID+"/logs?stderr=1&stdout=1&timestamps=1" {
		t.Errorf("last request = %q, want the versioned log request", last)
	}
}

func TestLogsQueryOptions(t *testing.T) {
	client, server := connect(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			w.Write(testdata(t, "version.json"))
			return
		}
		w.Write(frame(1, "line\n"))
	})
	logs, err := client.Logs(context.Background(), containerID, LogOptions{
		Follow: true,
		Since:  time.Unix(1725000000, 0),
	})
	if err != nil {
		t.Fatalf("Logs() error = %v", err)
	}
	defer logs.Close()
	if _, err := io.ReadAll(logs); err != nil {
		t.Fatalf("read logs: %v", err)
	}
	last := server.uris()[len(server.uris())-1]
	if last != "/v1.46/containers/"+containerID+"/logs?follow=1&since=1725000000&stderr=1&stdout=1" {
		t.Errorf("last request = %q, want follow and since", last)
	}
}

func TestLogsTTYPassesThrough(t *testing.T) {
	client, _ := connect(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			w.Write(testdata(t, "version.json"))
			return
		}
		w.Write([]byte("plain text without framing\n"))
	})
	logs, err := client.Logs(context.Background(), containerID, LogOptions{TTY: true})
	if err != nil {
		t.Fatalf("Logs() error = %v", err)
	}
	defer logs.Close()
	got, err := io.ReadAll(logs)
	if err != nil {
		t.Fatalf("read logs: %v", err)
	}
	if string(got) != "plain text without framing\n" {
		t.Errorf("logs = %q, want the unframed text", got)
	}
}

func TestLogsRejectsUnknownFraming(t *testing.T) {
	client, _ := connect(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			w.Write(testdata(t, "version.json"))
			return
		}
		w.Write([]byte{9, 0, 0, 0, 0, 0, 0, 2, 'h', 'i'})
	})
	logs, err := client.Logs(context.Background(), containerID, LogOptions{})
	if err != nil {
		t.Fatalf("Logs() error = %v", err)
	}
	defer logs.Close()
	_, err = io.ReadAll(logs)
	if err == nil || !strings.Contains(err.Error(), "unknown log stream type 9") {
		t.Errorf("read error = %v, want an unknown stream type", err)
	}
}

func TestEvents(t *testing.T) {
	client, server := connect(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			w.Write(testdata(t, "version.json"))
			return
		}
		w.Write(testdata(t, "events.jsonl"))
	})
	var events []Event
	err := client.Events(context.Background(), func(event Event) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatalf("Events() error = %v", err)
	}
	if len(events) != 5 {
		t.Fatalf("events = %d, want 5", len(events))
	}
	wantActions := []string{"create", "start", "health_status", "die", "destroy"}
	for i, want := range wantActions {
		if events[i].Action != want {
			t.Errorf("events[%d].Action = %q, want %q", i, events[i].Action, want)
		}
	}
	health := events[2]
	if health.Health != "healthy" {
		t.Errorf("health event Health = %q, want healthy", health.Health)
	}
	if health.Name != "shop-api-1" || health.Image != "shop/api:2.4.0" {
		t.Errorf("health event = %+v, want the recorded container", health)
	}
	if want := time.Unix(0, 1725000030123456789).UTC(); !health.Time.Equal(want) {
		t.Errorf("health event Time = %v, want %v", health.Time, want)
	}
	die := events[3]
	if die.ExitCode != 137 {
		t.Errorf("die event ExitCode = %d, want 137", die.ExitCode)
	}
	if die.Name != "shop-db-1" {
		t.Errorf("die event Name = %q, want shop-db-1", die.Name)
	}
	if events[0].ExitCode != -1 {
		t.Errorf("create event ExitCode = %d, want -1", events[0].ExitCode)
	}
	last := server.uris()[len(server.uris())-1]
	if last != "/v1.46/events?filters=%7B%22type%22%3A%5B%22container%22%5D%7D" {
		t.Errorf("last request = %q, want the container event filter", last)
	}
}

func TestEventsStopsOnContext(t *testing.T) {
	client, _ := connect(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			w.Write(testdata(t, "version.json"))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- client.Events(ctx, func(Event) {})
	}()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) && err == nil {
		t.Errorf("Events() error = %v, want the context error", err)
	}
}

func TestErrorResponseCarriesTheDaemonMessage(t *testing.T) {
	client, _ := connect(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			w.Write(testdata(t, "version.json"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"No such container: missing"}`))
	})
	_, err := client.Inspect(context.Background(), "missing")
	if err == nil {
		t.Fatal("Inspect() error = nil, want an error")
	}
	for _, want := range []string{"404 Not Found", "No such container: missing"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Inspect() error = %q, want it to contain %q", err.Error(), want)
		}
	}
}

func TestConnectRejectsBadHosts(t *testing.T) {
	tests := []struct {
		name string
		host string
	}{
		{name: "unsupported scheme", host: "http://127.0.0.1:2375"},
		{name: "no scheme", host: "127.0.0.1:2375"},
		{name: "empty unix path", host: "unix://"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Connect(context.Background(), tt.host); err == nil {
				t.Errorf("Connect(%q) error = nil, want an error", tt.host)
			}
		})
	}
}

func TestLogsRejectsHugeFrames(t *testing.T) {
	header := make([]byte, frameHeader)
	header[0] = 1
	binary.BigEndian.PutUint32(header[4:], maxFrame+1)
	reader := newDemuxReader(bytes.NewReader(header))
	if _, err := io.ReadAll(reader); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("read error = %v, want a frame size error", err)
	}
}

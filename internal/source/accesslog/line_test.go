package accesslog

import (
	"testing"
	"time"
)

const sampleLine = `{"ClientAddr":"172.20.0.9:41234","RequestHost":"shop.example.com","RequestMethod":"GET",` +
	`"RequestPath":"/api/orders/1042","OriginStatus":200,"DownstreamStatus":200,"DownstreamContentSize":512,` +
	`"Duration":13000000,"StartUTC":"2026-10-18T10:00:00.000000000Z","ServiceName":"shop-api",` +
	`"RouterName":"shop-api@docker","time":"2026-10-18T10:00:00.136456789Z"}`

func TestParseLine(t *testing.T) {
	line, ok := parseLine([]byte(sampleLine))
	if !ok {
		t.Fatal("parseLine() = not an access log line, want it parsed")
	}
	if want := time.Date(2026, 10, 18, 10, 0, 0, 136456789, time.UTC); !line.Time.Equal(want) {
		t.Errorf("Time = %v, want %v", line.Time, want)
	}
	if line.RouterName != "shop-api@docker" || line.ServiceName != "shop-api" || line.Host != "shop.example.com" {
		t.Errorf("line = %+v, want the router, service and host", line)
	}
	if line.Method != "GET" || line.Path != "/api/orders/1042" {
		t.Errorf("line = %+v, want the request method and path", line)
	}
	if line.Status != 200 || line.OriginStatus != 200 {
		t.Errorf("line = %+v, want both statuses", line)
	}
	if line.Duration != 13*time.Millisecond {
		t.Errorf("Duration = %v, want 13ms", line.Duration)
	}
	if line.Bytes != 512 {
		t.Errorf("Bytes = %d, want 512", line.Bytes)
	}
}

func TestParseLineWithoutTime(t *testing.T) {
	without := `{"RequestMethod":"GET","RequestPath":"/health","OriginStatus":200,"DownstreamStatus":200,` +
		`"Duration":2500000000,"StartUTC":"2026-10-18T10:00:00.000000000Z"}`
	line, ok := parseLine([]byte(without))
	if !ok {
		t.Fatal("parseLine() = not an access log line, want it parsed")
	}
	if want := time.Date(2026, 10, 18, 10, 0, 2, 500000000, time.UTC); !line.Time.Equal(want) {
		t.Errorf("Time = %v, want StartUTC plus Duration (%v)", line.Time, want)
	}
}

func TestParseLineRejectsOtherLines(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{name: "empty"},
		{name: "plain text", text: "2026-10-18T10:00:00Z INF Starting provider *file.Provider"},
		{name: "traefik log line", text: `{"level":"info","msg":"Configuration loaded","time":"2026-10-18T10:00:00Z"}`},
		{name: "malformed json", text: `{"DownstreamStatus":`},
		{name: "no status", text: `{"RouterName":"shop@docker","RequestPath":"/"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, ok := parseLine([]byte(tt.text)); ok {
				t.Errorf("parseLine(%q) = parsed, want it skipped", tt.text)
			}
		})
	}
}

func TestLineNoBackend(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		originStatus int
		want         bool
	}{
		{name: "bad gateway without a backend", status: 502, originStatus: 0, want: true},
		{name: "service unavailable without a backend", status: 503, originStatus: 0, want: true},
		{name: "gateway timeout without a backend", status: 504, originStatus: 0, want: true},
		{name: "bad gateway from the backend", status: 502, originStatus: 502},
		{name: "internal error", status: 500, originStatus: 500},
		{name: "success"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line := Line{Status: tt.status, OriginStatus: tt.originStatus}
			if got := line.noBackend(); got != tt.want {
				t.Errorf("noBackend() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLineCountLatency(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		status int
		ignore []string
		want   bool
	}{
		{name: "ordinary request", path: "/api/orders/1", status: 200, want: true},
		{name: "protocol upgrade", path: "/ws", status: 101},
		{name: "ignored path", path: "/ws/chat", status: 200, ignore: []string{"/ws"}},
		{name: "ignored path prefix", path: "/events/stream", status: 200, ignore: []string{"/ws", "/events"}},
		{name: "other path", path: "/api/events", status: 200, ignore: []string{"/ws"}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line := Line{Path: tt.path, Status: tt.status}
			if got := line.countLatency(tt.ignore); got != tt.want {
				t.Errorf("countLatency(%v) = %v, want %v", tt.ignore, got, tt.want)
			}
		})
	}
}

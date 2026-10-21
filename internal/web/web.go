// Package web serves the Raedar UI, the JSON API and the live container logs.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/mmrzaf/raedar/internal/clock"
	"github.com/mmrzaf/raedar/internal/config"
	"github.com/mmrzaf/raedar/internal/discovery"
	"github.com/mmrzaf/raedar/internal/docker"
	"github.com/mmrzaf/raedar/internal/store"
)

//go:embed templates/*.html static/*
var embedded embed.FS

// Model provides the live model of the host.
type Model interface {
	// Model returns the current discovery model.
	Model() *discovery.Model
}

// Options configure the web interface.
type Options struct {
	// Version is the Raedar version.
	Version string
	// DevStatic is a directory served instead of the embedded static files.
	DevStatic string
	// Heartbeat handles /heartbeat/ requests; nil when no peers are expected.
	Heartbeat http.Handler
}

// Server renders the pages and answers the JSON API.
type Server struct {
	store     *store.Store
	cfg       *config.Config
	connector *docker.Connector
	model     Model
	logger    *slog.Logger
	clock     clock.Clock
	version   string
	startedAt time.Time
	templates *template.Template
	static    fs.FS
	devStatic string
	heartbeat http.Handler
}

// New returns the web server.
func New(st *store.Store, cfg *config.Config, connector *docker.Connector, model Model, logger *slog.Logger, clock clock.Clock, options Options) (*Server, error) {
	templates, err := template.New("").Funcs(templateFuncs(cfg.Location, clock, cfg.SubjectNames.Display)).ParseFS(embedded, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	static, err := fs.Sub(embedded, "static")
	if err != nil {
		return nil, fmt.Errorf("read static files: %w", err)
	}
	server := &Server{
		store:     st,
		cfg:       cfg,
		connector: connector,
		model:     model,
		logger:    logger,
		clock:     clock,
		version:   options.Version,
		startedAt: clock.Now(),
		templates: templates,
		static:    static,
		devStatic: options.DevStatic,
		heartbeat: options.Heartbeat,
	}
	return server, nil
}

// Handler returns the HTTP handler of the web interface.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.overviewPage)
	mux.HandleFunc("GET /apps/{app}", s.appPage)
	mux.HandleFunc("GET /host", s.hostPage)
	mux.HandleFunc("GET /patterns", s.patternsPage)
	mux.HandleFunc("GET /events", s.eventsPage)
	mux.HandleFunc("GET /silences", s.silencesPage)
	mux.HandleFunc("POST /silences", s.createSilence)
	mux.HandleFunc("POST /silences/{id}/delete", s.deleteSilence)
	mux.HandleFunc("GET /deliveries", s.deliveriesPage)
	mux.HandleFunc("GET /incidents/{id}", s.incidentPage)
	mux.HandleFunc("GET /about", s.aboutPage)
	mux.HandleFunc("GET /api/overview", s.overviewAPI)
	mux.HandleFunc("GET /api/series", s.seriesAPI)
	mux.HandleFunc("GET /api/baseline", s.baselineAPI)
	mux.HandleFunc("GET /api/paths", s.pathsAPI)
	mux.HandleFunc("GET /api/events", s.eventsAPI)
	mux.HandleFunc("GET /api/patterns", s.patternsAPI)
	mux.HandleFunc("GET /api/logs", s.logsAPI)
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /static/", s.serveStatic)
	if s.heartbeat != nil {
		mux.Handle("/heartbeat/", s.heartbeat)
	}
	return withSecurityHeaders(mux)
}

// contentSecurityPolicy limits every page to resources served by Raedar
// itself. The pages load no scripts, styles, fonts or images from elsewhere,
// and this keeps it that way.
const contentSecurityPolicy = "default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// withSecurityHeaders adds the headers every response carries.
func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		header := w.Header()
		header.Set("Content-Security-Policy", contentSecurityPolicy)
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, request)
	})
}

// serveStatic serves the embedded or development static files.
func (s *Server) serveStatic(w http.ResponseWriter, request *http.Request) {
	name := strings.TrimPrefix(request.URL.Path, "/static/")
	if name == "" || strings.Contains(name, "..") {
		http.NotFound(w, request)
		return
	}
	var (
		data []byte
		err  error
	)
	if s.devStatic != "" {
		data, err = os.ReadFile(path.Join(s.devStatic, path.Clean("/"+name)))
	} else {
		data, err = fs.ReadFile(s.static, name)
	}
	if err != nil {
		http.NotFound(w, request)
		return
	}
	switch path.Ext(name) {
	case ".css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case ".ico":
		w.Header().Set("Content-Type", "image/x-icon")
	case ".js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(data)
}

// healthz reports whether the process runs and the database is reachable.
func (s *Server) healthz(w http.ResponseWriter, request *http.Request) {
	if s.store == nil {
		http.Error(w, "no database", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 3*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("ok\n"))
}

// templateFuncs returns the helpers the templates use.
func templateFuncs(location *time.Location, clock clock.Clock, names func(string) string) template.FuncMap {
	return template.FuncMap{
		"time": func(at time.Time) string {
			return at.In(location).Format("15:04")
		},
		"datetime": func(at time.Time) string {
			return at.In(location).Format("2006-01-02 15:04")
		},
		"relative": func(at time.Time) string {
			return relativeTime(clock.Now(), at)
		},
		"duration": durationText,
		"bytes":    bytesText,
		"count":    countText,
		"cpu":      cpuText,
		"rate":     rateText,
		"forecast": forecastText,
		"plural":   plural,
		"level":    meterLevel,
		"latency":  latencyText,
		"subject":  func(subject string) string { return names(subject) },
		"mulf": func(value, factor float64) float64 {
			return value * factor
		},
	}
}

// parseTime parses a query timestamp, in Unix seconds or RFC3339.
func parseTime(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		return time.Unix(seconds, 0).UTC(), true
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed.UTC(), true
	}
	return time.Time{}, false
}

// parseInt parses a query integer.
func parseInt(value string, fallback int) int {
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

// badRequest writes a parameter error and returns false.
func badRequest(w http.ResponseWriter, message string) bool {
	http.Error(w, message, http.StatusBadRequest)
	return false
}

// writeJSON writes a JSON response.
func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		http.Error(w, "cannot encode the response", http.StatusInternalServerError)
	}
}

// dedupe returns the values without duplicates, keeping their order.
func dedupe(values []string) []string {
	seen := make(map[string]bool, len(values))
	var result []string
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

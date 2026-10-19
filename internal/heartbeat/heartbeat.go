// Package heartbeat sends health heartbeats to an external URL and accepts
// them from peer Raedar instances.
package heartbeat

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/mmrzaf/raedar/internal/clock"
	"github.com/mmrzaf/raedar/internal/config"
)

// peerInterval is the shortest time between two accepted heartbeats of one
// peer.
const peerInterval = 5 * time.Second

// hitTimeout bounds one heartbeat request.
const hitTimeout = 15 * time.Second

// Sender pings an external URL while Raedar is healthy.
type Sender struct {
	url      string
	header   *config.Header
	interval time.Duration
	client   *http.Client
	logger   *slog.Logger
	clock    clock.Clock
	healthy  func(context.Context) bool
}

// NewSender returns a heartbeat sender. healthy reports whether Raedar may
// report itself alive.
func NewSender(cfg *config.Config, client *http.Client, logger *slog.Logger, clock clock.Clock, healthy func(context.Context) bool) *Sender {
	return &Sender{
		url:      cfg.Heartbeat.URL,
		header:   cfg.Heartbeat.Header,
		interval: cfg.Heartbeat.Interval,
		client:   client,
		logger:   logger,
		clock:    clock,
		healthy:  healthy,
	}
}

// Run sends heartbeats every interval until ctx is done.
func (s *Sender) Run(ctx context.Context) error {
	if s.url == "" {
		return nil
	}
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.beat(ctx)
		}
	}
}

// beat sends one heartbeat when Raedar is healthy.
func (s *Sender) beat(ctx context.Context) {
	if s.healthy != nil && !s.healthy(ctx) {
		s.logger.Debug("heartbeat skipped, Raedar is not healthy")
		return
	}
	requestCtx, cancel := context.WithTimeout(ctx, hitTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, s.url, nil)
	if err != nil {
		s.logger.Warn("build heartbeat request", slog.String("error", err.Error()))
		return
	}
	if s.header != nil {
		request.Header.Set(s.header.Name, s.header.Value)
	}
	response, err := s.client.Do(request)
	if err != nil {
		s.logger.Warn("heartbeat failed", slog.String("error", safeError(err).Error()))
		return
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, io.LimitReader(response.Body, 1<<16)); err != nil {
		s.logger.Warn("read heartbeat response", slog.String("error", err.Error()))
		return
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		s.logger.Warn("heartbeat failed", slog.String("status", response.Status))
	}
}

// Receiver accepts heartbeats from peers.
type Receiver struct {
	peers  []string
	token  string
	store  Store
	logger *slog.Logger
	clock  clock.Clock

	mu   sync.Mutex
	last map[string]time.Time
}

// Store records heartbeats.
type Store interface {
	// RecordHeartbeat records that a peer reported.
	RecordHeartbeat(ctx context.Context, name string, at time.Time) error
}

// NewReceiver returns a heartbeat receiver for the configured peers.
func NewReceiver(peers []string, token string, st Store, logger *slog.Logger, clock clock.Clock) *Receiver {
	return &Receiver{
		peers:  peers,
		token:  token,
		store:  st,
		logger: logger,
		clock:  clock,
		last:   make(map[string]time.Time),
	}
}

// ServeHTTP handles GET and POST /heartbeat/<name>.
func (r *Receiver) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(request.URL.Path, "/heartbeat/")
	if !r.known(name) {
		http.Error(w, "unknown peer", http.StatusNotFound)
		return
	}
	if !r.authorized(request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	now := r.clock.Now()
	r.mu.Lock()
	last, seen := r.last[name]
	tooSoon := seen && now.Sub(last) < peerInterval
	if !tooSoon {
		r.last[name] = now
	}
	r.mu.Unlock()
	if tooSoon {
		http.Error(w, "too many heartbeats", http.StatusTooManyRequests)
		return
	}
	if err := r.store.RecordHeartbeat(request.Context(), name, now); err != nil {
		r.logger.Warn("record heartbeat", slog.String("peer", name), slog.String("error", err.Error()))
		http.Error(w, "cannot record the heartbeat", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// known reports whether a peer is configured.
func (r *Receiver) known(name string) bool {
	for _, peer := range r.peers {
		if peer == name {
			return true
		}
	}
	return false
}

// authorized compares the bearer token in constant time.
func (r *Receiver) authorized(request *http.Request) bool {
	if r.token == "" {
		return false
	}
	header := request.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}
	token := header[len(prefix):]
	return subtle.ConstantTimeCompare([]byte(token), []byte(r.token)) == 1
}

// safeError removes the request URL from an error, because heartbeat URLs can
// contain secrets.
func safeError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("%s: %w", urlErr.Op, urlErr.Err)
	}
	return err
}

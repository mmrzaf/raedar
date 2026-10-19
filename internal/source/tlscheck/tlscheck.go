// Package tlscheck checks the certificates Traefik serves for every routed
// domain.
package tlscheck

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/mmrzaf/raedar/internal/discovery"
	"github.com/mmrzaf/raedar/internal/store"
)

// SourceName names the TLS checker in the source state.
const SourceName = "tlscheck"

const (
	// interval is how often a domain is checked.
	interval = 6 * time.Hour
	// jitter is the window over which checks of different domains are
	// spread.
	jitter = time.Hour
	// pollInterval is how often due domains are looked for.
	pollInterval = time.Minute
	// dialTimeout bounds one TLS handshake.
	dialTimeout = 10 * time.Second
)

// The certificate statuses.
const (
	// StatusOK means the certificate is valid and more than RAEDAR_CERT_WARN
	// from expiry.
	StatusOK = "ok"
	// StatusExpiring means the certificate expires within RAEDAR_CERT_WARN.
	StatusExpiring = "expiring"
	// StatusExpired means the certificate is past its expiry date.
	StatusExpired = "expired"
	// StatusDefault means Traefik served its built-in default certificate, so
	// no certificate matched the domain.
	StatusDefault = "default"
	// StatusMismatch means the certificate does not cover the domain.
	StatusMismatch = "mismatch"
	// StatusPrivate means the chain does not verify against public roots.
	StatusPrivate = "private"
	// StatusError means the connection or handshake failed.
	StatusError = "error"
)

// defaultCertificate is the common name of Traefik's built-in certificate.
const defaultCertificate = "TRAEFIK DEFAULT CERT"

// Clock provides timers to the checker, so that tests run without waiting.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// After returns a channel that receives the current time after d.
	After(d time.Duration) <-chan time.Time
}

// SystemClock returns the clock used outside tests.
func SystemClock() Clock {
	return systemClock{}
}

// systemClock is the clock of the running process.
type systemClock struct{}

// Now returns the current time in UTC.
func (systemClock) Now() time.Time {
	return time.Now().UTC()
}

// After returns a channel that receives the current time after d.
func (systemClock) After(d time.Duration) <-chan time.Time {
	return time.After(d)
}

// Model provides the domains of the TLS routes.
type Model interface {
	// TLSDomains returns the domains that need a certificate check, with a
	// route that serves each.
	TLSDomains() []discovery.TLSDomain
	// TLSCheckAddr returns the detected address where Traefik serves HTTPS,
	// empty when it could not be detected.
	TLSCheckAddr() string
}

// Store records the checks.
type Store interface {
	// UpsertCert records the latest check of one domain.
	UpsertCert(ctx context.Context, cert store.Cert) error
	// RecordSourceStatus records the state of one source.
	RecordSourceStatus(ctx context.Context, source, status, lastError string, at time.Time) error
}

// Checker checks the certificates of every routed domain.
type Checker struct {
	address string
	model   Model
	extra   []string
	store   Store
	logger  *slog.Logger
	clock   Clock
	warn    time.Duration

	interval time.Duration
	poll     time.Duration
	spread   func(time.Duration) time.Duration

	mu          sync.Mutex
	next        map[string]time.Time
	status      string
	statusError string
}

// New returns a certificate checker. address is the configured Traefik HTTPS
// address; when empty the detected one is used. extra are additional domains
// to check.
func New(address string, model Model, extra []string, st Store, logger *slog.Logger, clock Clock, warn time.Duration) *Checker {
	return &Checker{
		address:  address,
		model:    model,
		extra:    extra,
		store:    st,
		logger:   logger,
		clock:    clock,
		warn:     warn,
		interval: interval,
		poll:     pollInterval,
		spread:   randomSpread,
		next:     make(map[string]time.Time),
		status:   store.StatusUnavailable,
	}
}

// Run checks the due domains every poll until ctx is done.
func (c *Checker) Run(ctx context.Context) error {
	next := c.clock.After(0)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-next:
		}
		c.checkDue(ctx)
		next = c.clock.After(c.poll)
	}
}

// checkDue checks the domains whose next check is due.
func (c *Checker) checkDue(ctx context.Context) {
	address := c.checkAddress()
	if address == "" {
		c.recordStatus(ctx, store.StatusUnavailable, errors.New("no TLS check address: set RAEDAR_TLS_CHECK_ADDR"))
		return
	}
	now := c.clock.Now()
	due := 0
	failures := 0
	for _, domain := range c.domains() {
		c.mu.Lock()
		at, seen := c.next[domain.Name]
		dueNow := !seen || !now.Before(at)
		c.mu.Unlock()
		if !dueNow {
			continue
		}
		due++
		c.mu.Lock()
		c.next[domain.Name] = now.Add(c.interval + c.spread(c.jitter()))
		c.mu.Unlock()
		if err := c.check(ctx, address, domain, now); err != nil {
			failures++
			c.logger.Warn("check certificate", slog.String("domain", domain.Name), slog.String("error", err.Error()))
		}
	}
	if due == 0 {
		return
	}
	if failures > 0 {
		c.recordStatus(ctx, store.StatusDegraded, fmt.Errorf("%d of %d domains could not be checked", failures, due))
		return
	}
	c.recordStatus(ctx, store.StatusOK, nil)
}

// jitter returns the jitter window.
func (c *Checker) jitter() time.Duration {
	return jitter
}

// domains returns the domains to check: every domain of a TLS route plus the
// configured extra domains.
func (c *Checker) domains() []discovery.TLSDomain {
	seen := make(map[string]bool)
	var domains []discovery.TLSDomain
	for _, domain := range c.model.TLSDomains() {
		if domain.Name == "" || seen[domain.Name] {
			continue
		}
		seen[domain.Name] = true
		domains = append(domains, domain)
	}
	for _, name := range c.extra {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		domains = append(domains, discovery.TLSDomain{Name: name})
	}
	return domains
}

// checkAddress returns the address to check: the configured one, or the one
// detected from the Traefik container.
func (c *Checker) checkAddress() string {
	if c.address != "" {
		return c.address
	}
	return c.model.TLSCheckAddr()
}

// check checks one domain and records the result. A failed handshake is
// recorded as the domain's error status and reported as a failed check.
func (c *Checker) check(ctx context.Context, address string, domain discovery.TLSDomain, now time.Time) error {
	state, err := handshake(ctx, address, domain.Name)
	if err != nil {
		if recordErr := c.record(ctx, store.Cert{
			Domain:    domain.Name,
			RouteID:   routeIDOf(domain),
			CheckedAt: now,
			Status:    StatusError,
			Error:     err.Error(),
		}); recordErr != nil {
			return recordErr
		}
		return fmt.Errorf("check %s: %w", domain.Name, err)
	}
	result := Evaluate(state, domain.Name, now, c.warn, nil)
	result.RouteID = routeIDOf(domain)
	result.CheckedAt = now
	return c.record(ctx, result)
}

// record stores one check.
func (c *Checker) record(ctx context.Context, cert store.Cert) error {
	if err := c.store.UpsertCert(ctx, cert); err != nil {
		return err
	}
	return nil
}

// handshake performs a TLS handshake with Traefik for one domain.
func handshake(ctx context.Context, address, domain string) (tls.ConnectionState, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	dialer := &tls.Dialer{Config: &tls.Config{
		ServerName:         domain,
		InsecureSkipVerify: true, // The certificate is evaluated by Evaluate.
	}}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return tls.ConnectionState{}, err
	}
	defer conn.Close()
	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		return tls.ConnectionState{}, errors.New("connection is not TLS")
	}
	return tlsConn.ConnectionState(), nil
}

// Evaluate decides the check result of the certificate served for a domain.
// The state comes from a handshake that did not verify the chain; Evaluate
// performs the checks itself. A nil roots pool means the system roots.
func Evaluate(state tls.ConnectionState, domain string, now time.Time, warn time.Duration, roots *x509.CertPool) store.Cert {
	cert := store.Cert{Domain: domain, Status: StatusError}
	if len(state.PeerCertificates) == 0 {
		cert.Error = "the server sent no certificate"
		return cert
	}
	leaf := state.PeerCertificates[0]
	cert.NotAfter = &leaf.NotAfter
	cert.Issuer = leaf.Issuer.String()
	cert.Subject = leaf.Subject.String()
	cert.SANs = append([]string(nil), leaf.DNSNames...)
	switch {
	case isDefaultCertificate(leaf):
		cert.Status = StatusDefault
	case leaf.VerifyHostname(domain) != nil:
		cert.Status = StatusMismatch
	case now.After(leaf.NotAfter):
		cert.Status = StatusExpired
	case verifyChain(leaf, state.PeerCertificates[1:], domain, roots) != nil:
		cert.Status = StatusPrivate
	case leaf.NotAfter.Sub(now) <= warn:
		cert.Status = StatusExpiring
	default:
		cert.Status = StatusOK
	}
	return cert
}

// isDefaultCertificate reports whether a certificate is Traefik's built-in
// default certificate.
func isDefaultCertificate(leaf *x509.Certificate) bool {
	return strings.Contains(leaf.Issuer.CommonName, defaultCertificate) ||
		strings.Contains(leaf.Subject.CommonName, defaultCertificate)
}

// verifyChain verifies a certificate and its intermediates against the roots.
func verifyChain(leaf *x509.Certificate, intermediates []*x509.Certificate, domain string, roots *x509.CertPool) error {
	pool := x509.NewCertPool()
	for _, intermediate := range intermediates {
		pool.AddCert(intermediate)
	}
	_, err := leaf.Verify(x509.VerifyOptions{DNSName: domain, Intermediates: pool, Roots: roots})
	return err
}

// routeIDOf returns the route ID of a domain, nil when none is known.
func routeIDOf(domain discovery.TLSDomain) *int64 {
	if domain.RouteID == 0 {
		return nil
	}
	id := domain.RouteID
	return &id
}

// randomSpread returns a random delay below the window.
func randomSpread(window time.Duration) time.Duration {
	if window <= 0 {
		return 0
	}
	return rand.N(window)
}

// recordStatus stores the state of the source when it changed.
func (c *Checker) recordStatus(ctx context.Context, status string, cause error) {
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	c.mu.Lock()
	changed := c.status != status || c.statusError != message
	c.status = status
	c.statusError = message
	c.mu.Unlock()
	if !changed {
		return
	}
	if err := c.store.RecordSourceStatus(ctx, SourceName, status, message, c.clock.Now()); err != nil {
		c.logger.Warn("record TLS source status", slog.String("error", err.Error()))
	}
}

package tlscheck

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/mmrzaf/raedar/internal/discovery"
	"github.com/mmrzaf/raedar/internal/store"
)

// testClock is a clock the tests move by hand.
type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time { return c.now }
func (c *testClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	ch <- c.now.Add(d)
	return ch
}

// fakeStore records certificate checks.
type fakeStore struct {
	mu      sync.Mutex
	certs   map[string]store.Cert
	order   []string
	status  string
	lastErr string
}

func (f *fakeStore) UpsertCert(ctx context.Context, cert store.Cert) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.certs == nil {
		f.certs = make(map[string]store.Cert)
	}
	f.certs[cert.Domain] = cert
	f.order = append(f.order, cert.Domain)
	return nil
}

func (f *fakeStore) RecordSourceStatus(ctx context.Context, source, status, lastError string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.lastErr = status, lastError
	return nil
}

// certOf returns the check of one domain.
func (f *fakeStore) certOf(t *testing.T, domain string) store.Cert {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	cert, ok := f.certs[domain]
	if !ok {
		t.Fatalf("no certificate recorded for %s", domain)
	}
	return cert
}

// statusOf returns the recorded status.
func (f *fakeStore) statusOf() (string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status, f.lastErr
}

// fakeModel returns a fixed set of domains.
type fakeModel struct {
	domains []discovery.TLSDomain
	addr    string
}

func (m *fakeModel) TLSDomains() []discovery.TLSDomain { return m.domains }
func (m *fakeModel) TLSCheckAddr() string              { return m.addr }

// discardLogger returns a logger that writes nothing.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testCertificate is a generated certificate with the pool that trusts its
// issuer.
type testCertificate struct {
	certificate tls.Certificate
	roots       *x509.CertPool
}

// issue creates a CA and a leaf certificate for the given names.
func issue(t *testing.T, commonName string, dnsNames []string, notBefore, notAfter time.Time) testCertificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	ca := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test CA"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA: %v", err)
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: commonName},
		DNSNames:     dnsNames,
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create leaf: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	return testCertificate{
		certificate: tls.Certificate{
			Certificate: [][]byte{leafDER, caDER},
			PrivateKey:  key,
			Leaf:        nil,
		},
		roots: roots,
	}
}

// selfSigned creates a self-signed certificate for the given common name.
func selfSigned(t *testing.T, commonName string, notBefore, notAfter time.Time) testCertificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: commonName},
		Issuer:                pkix.Name{CommonName: commonName},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return testCertificate{
		certificate: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key},
		roots:       x509.NewCertPool(),
	}
}

// state returns the TLS connection state of a certificate.
func (c testCertificate) state(t *testing.T) tls.ConnectionState {
	t.Helper()
	certificates := make([]*x509.Certificate, 0, len(c.certificate.Certificate))
	for _, der := range c.certificate.Certificate {
		certificate, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatalf("parse certificate: %v", err)
		}
		certificates = append(certificates, certificate)
	}
	return tls.ConnectionState{PeerCertificates: certificates}
}

func TestEvaluate(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	warn := 14 * 24 * time.Hour
	tests := []struct {
		name    string
		cert    testCertificate
		domain  string
		roots   *x509.CertPool
		want    string
		wantErr string
	}{
		{
			name:   "valid",
			cert:   issue(t, "example.com", []string{"example.com"}, now.Add(-time.Hour), now.Add(90*24*time.Hour)),
			domain: "example.com",
			want:   StatusOK,
		},
		{
			name:   "wildcard",
			cert:   issue(t, "*.example.com", []string{"*.example.com"}, now.Add(-time.Hour), now.Add(90*24*time.Hour)),
			domain: "shop.example.com",
			want:   StatusOK,
		},
		{
			name:   "expiring",
			cert:   issue(t, "example.com", []string{"example.com"}, now.Add(-time.Hour), now.Add(7*24*time.Hour)),
			domain: "example.com",
			want:   StatusExpiring,
		},
		{
			name:   "expired",
			cert:   issue(t, "example.com", []string{"example.com"}, now.Add(-48*time.Hour), now.Add(-time.Hour)),
			domain: "example.com",
			want:   StatusExpired,
		},
		{
			name:   "mismatch",
			cert:   issue(t, "other.example.com", []string{"other.example.com"}, now.Add(-time.Hour), now.Add(90*24*time.Hour)),
			domain: "example.com",
			want:   StatusMismatch,
		},
		{
			name:   "default certificate",
			cert:   selfSigned(t, defaultCertificate, now.Add(-time.Hour), now.Add(90*24*time.Hour)),
			domain: "example.com",
			want:   StatusDefault,
		},
		{
			name:   "private chain",
			cert:   issue(t, "example.com", []string{"example.com"}, now.Add(-time.Hour), now.Add(90*24*time.Hour)),
			domain: "example.com",
			roots:  x509.NewCertPool(),
			want:   StatusPrivate,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			roots := tt.roots
			if roots == nil {
				roots = tt.cert.roots
			}
			got := Evaluate(tt.cert.state(t), tt.domain, now, warn, roots)
			if got.Status != tt.want {
				t.Errorf("status = %q, want %q", got.Status, tt.want)
			}
			if got.NotAfter == nil {
				t.Error("NotAfter = nil, want the certificate's expiry")
			}
		})
	}
}

func TestEvaluateKeepsTheCertificateDetails(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	certificate := issue(t, "example.com", []string{"example.com", "www.example.com"}, now.Add(-time.Hour), now.Add(90*24*time.Hour))
	got := Evaluate(certificate.state(t), "example.com", now, 14*24*time.Hour, certificate.roots)
	if len(got.SANs) != 2 || got.SANs[0] != "example.com" || got.SANs[1] != "www.example.com" {
		t.Errorf("SANs = %v, want the certificate's names", got.SANs)
	}
	if got.Issuer == "" || got.Subject == "" {
		t.Errorf("certificate = %+v, want issuer and subject", got)
	}
}

func TestEvaluateWithoutCertificate(t *testing.T) {
	got := Evaluate(tls.ConnectionState{}, "example.com", time.Now(), time.Hour, nil)
	if got.Status != StatusError || got.Error == "" {
		t.Errorf("Evaluate() = %+v, want an error status", got)
	}
}

func TestCheckAgainstServer(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	expired := issue(t, "example.com", []string{"example.com"}, now.Add(-48*time.Hour), now.Add(-24*time.Hour))
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{expired.certificate}}
	server.StartTLS()
	defer server.Close()
	st := &fakeStore{}
	model := &fakeModel{domains: []discovery.TLSDomain{{Name: "example.com", RouteID: 9}}}
	checker := New(server.Listener.Addr().String(), model, nil, st, discardLogger(), &testClock{now: now}, 14*24*time.Hour)
	checker.spread = func(time.Duration) time.Duration { return 0 }
	checker.checkDue(context.Background())
	cert := st.certOf(t, "example.com")
	if cert.Status != StatusExpired {
		t.Errorf("status = %q, want expired", cert.Status)
	}
	if cert.RouteID == nil || *cert.RouteID != 9 {
		t.Errorf("route = %v, want 9", cert.RouteID)
	}
	if !cert.CheckedAt.Equal(now) {
		t.Errorf("checked at = %v, want %v", cert.CheckedAt, now)
	}
	if cert.Issuer == "" || cert.Subject == "" {
		t.Errorf("certificate = %+v, want issuer and subject", cert)
	}
	if status, _ := st.statusOf(); status != "ok" {
		t.Errorf("status = %q, want ok", status)
	}
}

func TestCheckRecordsHandshakeErrors(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	st := &fakeStore{}
	model := &fakeModel{domains: []discovery.TLSDomain{{Name: "example.com"}}}
	// Port 1 is closed, so the handshake fails.
	checker := New("127.0.0.1:1", model, nil, st, discardLogger(), &testClock{now: now}, 14*24*time.Hour)
	checker.spread = func(time.Duration) time.Duration { return 0 }
	checker.checkDue(context.Background())
	cert := st.certOf(t, "example.com")
	if cert.Status != StatusError || cert.Error == "" {
		t.Errorf("certificate = %+v, want an error status", cert)
	}
	status, _ := st.statusOf()
	if status != "degraded" {
		t.Errorf("status = %q, want degraded", status)
	}
}

func TestCheckWithoutAddress(t *testing.T) {
	st := &fakeStore{}
	checker := New("", &fakeModel{}, nil, st, discardLogger(), &testClock{now: time.Now()}, 14*24*time.Hour)
	checker.checkDue(context.Background())
	status, lastError := st.statusOf()
	if status != "unavailable" {
		t.Errorf("status = %q, want unavailable", status)
	}
	if lastError == "" {
		t.Error("last error is empty, want the missing address")
	}
}

func TestChecksAreSpreadOverTheInterval(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	certificate := issue(t, "example.com", []string{"example.com"}, now.Add(-time.Hour), now.Add(90*24*time.Hour))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate.certificate}}
	server.StartTLS()
	defer server.Close()
	st := &fakeStore{}
	model := &fakeModel{domains: []discovery.TLSDomain{{Name: "example.com"}}}
	clock := &testClock{now: now}
	checker := New(server.Listener.Addr().String(), model, nil, st, discardLogger(), clock, 14*24*time.Hour)
	checker.spread = func(time.Duration) time.Duration { return 0 }
	checker.checkDue(context.Background())
	if len(st.order) != 1 {
		t.Fatalf("checks = %v, want one", st.order)
	}
	// Not due again within the interval.
	clock.now = now.Add(time.Hour)
	checker.checkDue(context.Background())
	if len(st.order) != 1 {
		t.Errorf("checks = %v, want none before the interval", st.order)
	}
	clock.now = now.Add(7 * time.Hour)
	checker.checkDue(context.Background())
	if len(st.order) != 2 {
		t.Errorf("checks = %v, want a second check after the interval", st.order)
	}
}

func TestExtraDomains(t *testing.T) {
	model := &fakeModel{domains: []discovery.TLSDomain{{Name: "a.example.com", RouteID: 1}}}
	checker := New("127.0.0.1:443", model, []string{"b.example.com", "a.example.com"}, &fakeStore{}, discardLogger(), &testClock{}, time.Hour)
	domains := checker.domains()
	if len(domains) != 2 {
		t.Fatalf("domains = %+v, want two distinct domains", domains)
	}
	if domains[0].Name != "a.example.com" || domains[0].RouteID != 1 {
		t.Errorf("first domain = %+v, want the route domain", domains[0])
	}
	if domains[1].Name != "b.example.com" || domains[1].RouteID != 0 {
		t.Errorf("second domain = %+v, want the extra domain without a route", domains[1])
	}
}

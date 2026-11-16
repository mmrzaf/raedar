package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/mmrzaf/raedar/internal/store"
)

// Outcome is the result of one check.
type Outcome struct {
	// Result is healthy, unhealthy or unreachable.
	Result string
	// Status is the HTTP status; zero without a response.
	Status int
	// Latency is the time until the response headers; zero without a response.
	Latency time.Duration
	// Error describes a failure.
	Error string
}

// Client sends checks. Every request goes to one fixed address, Traefik's
// HTTPS entry point; the target's hostname only selects the route.
type Client struct {
	http *http.Client
}

// NewClient returns a client that connects only to address. roots is nil for
// the system roots. Redirects are never followed, nothing but a User-Agent is
// sent, and the response body is never read.
func NewClient(address, userAgent string, timeout time.Duration, roots *x509.CertPool) *Client {
	dialer := &net.Dialer{Timeout: timeout}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, address)
		},
		TLSClientConfig:     &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: timeout,
		DisableKeepAlives:   true,
	}
	return &Client{http: &http.Client{
		Transport:     userAgentTransport{base: transport, userAgent: userAgent},
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// userAgentTransport sets the only header a check carries.
type userAgentTransport struct {
	base      http.RoundTripper
	userAgent string
}

func (t userAgentTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	request.Header = http.Header{"User-Agent": {t.userAgent}, "Accept": {"*/*"}}
	return t.base.RoundTrip(request)
}

// Check requests the target once and classifies the answer.
func (c *Client) Check(ctx context.Context, target Target) Outcome {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+target.Host+target.Path, nil)
	if err != nil {
		return Outcome{Result: store.ProbeUnreachable, Error: err.Error()}
	}
	started := time.Now()
	response, err := c.http.Do(request)
	if err != nil {
		return Outcome{Result: store.ProbeUnreachable, Error: describe(err)}
	}
	_ = response.Body.Close()
	outcome := Outcome{Status: response.StatusCode, Latency: time.Since(started)}
	if target.Expect.Accepts(response.StatusCode) {
		outcome.Result = store.ProbeHealthy
		return outcome
	}
	outcome.Result = store.ProbeUnhealthy
	outcome.Error = fmt.Sprintf("status %d is not expected", response.StatusCode)
	return outcome
}

// describe shortens a transport error to what an operator can act on.
func describe(err error) string {
	var unknownAuthority x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	var hostname x509.HostnameError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &unknownAuthority), errors.As(err, &invalid), errors.As(err, &hostname):
		return "TLS certificate not accepted"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return "connection failed: " + opErr.Op
	}
	return "request failed"
}

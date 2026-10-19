package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mmrzaf/raedar/internal/config"
)

const (
	// requestTimeout bounds every notification request.
	requestTimeout = 15 * time.Second
	// dialTimeout bounds opening a connection, including through a proxy.
	dialTimeout = 10 * time.Second
)

// OutboundClient returns the HTTP client used for notifications and the
// outgoing heartbeat. When useProxy is true and a proxy is configured,
// requests go through it. The standard proxy variables are ignored on purpose.
func OutboundClient(cfg *config.Config, useProxy bool) (*http.Client, error) {
	dialer := &net.Dialer{Timeout: dialTimeout}
	transport := &http.Transport{
		DialContext:         dialer.DialContext,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
	}
	if useProxy && cfg.Notify.OutboundProxy != "" {
		proxy, err := url.Parse(cfg.Notify.OutboundProxy)
		if err != nil {
			return nil, fmt.Errorf("parse outbound proxy: %w", err)
		}
		switch strings.ToLower(proxy.Scheme) {
		case "http":
			transport.Proxy = http.ProxyURL(proxy)
		case "socks5":
			socks, err := newSOCKS5Dialer(proxy)
			if err != nil {
				return nil, err
			}
			transport.DialContext = socks.DialContext
		default:
			return nil, errors.New("outbound proxy must be an http:// or socks5:// URL")
		}
	}
	return &http.Client{Transport: transport, Timeout: requestTimeout}, nil
}

// socks5Dialer dials through a SOCKS5 proxy, with optional user name and
// password authentication.
type socks5Dialer struct {
	address  string
	username string
	password string
	dialer   *net.Dialer
}

// newSOCKS5Dialer returns a dialer for a socks5:// URL.
func newSOCKS5Dialer(proxy *url.URL) (*socks5Dialer, error) {
	if proxy.Host == "" {
		return nil, errors.New("outbound proxy has no host")
	}
	dialer := &socks5Dialer{address: proxy.Host, dialer: &net.Dialer{Timeout: dialTimeout}}
	if proxy.User != nil {
		dialer.username = proxy.User.Username()
		dialer.password, _ = proxy.User.Password()
	}
	return dialer, nil
}

// DialContext connects to address through the proxy.
func (d *socks5Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("SOCKS5 supports TCP only, not %s", network)
	}
	conn, err := d.dialer.DialContext(ctx, "tcp", d.address)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	deadline := time.Now().Add(dialTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		conn.Close()
		return nil, err
	}
	if err := d.handshake(conn, address); err != nil {
		conn.Close()
		return nil, err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// handshake performs the SOCKS5 greeting, authentication and connect.
func (d *socks5Dialer) handshake(conn net.Conn, address string) error {
	methods := []byte{0x00} // no authentication
	if d.username != "" {
		methods = []byte{0x00, 0x02}
	}
	if _, err := conn.Write(append([]byte{0x05, byte(len(methods))}, methods...)); err != nil {
		return err
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return err
	}
	if reply[0] != 0x05 {
		return errors.New("SOCKS5 proxy answered with another version")
	}
	switch reply[1] {
	case 0x00:
	case 0x02:
		if d.username == "" {
			return errors.New("SOCKS5 proxy requires authentication")
		}
		if err := d.authenticate(conn); err != nil {
			return err
		}
	default:
		return errors.New("SOCKS5 proxy accepts no supported authentication method")
	}
	return d.connect(conn, address)
}

// authenticate performs user name and password authentication.
func (d *socks5Dialer) authenticate(conn net.Conn) error {
	request := []byte{0x01, byte(len(d.username))}
	request = append(request, d.username...)
	request = append(request, byte(len(d.password)))
	request = append(request, d.password...)
	if _, err := conn.Write(request); err != nil {
		return err
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return err
	}
	if reply[1] != 0x00 {
		return errors.New("SOCKS5 proxy rejected the credentials")
	}
	return nil
}

// connect asks the proxy for the target address.
func (d *socks5Dialer) connect(conn net.Conn, address string) error {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid port %q", portText)
	}
	request := []byte{0x05, 0x01, 0x00} // connect, IPv4
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			request = append(request, 0x01)
			request = append(request, ip4...)
		} else {
			request = append(request, 0x04)
			request = append(request, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return errors.New("host name is too long for SOCKS5")
		}
		request = append(request, 0x03, byte(len(host)))
		request = append(request, host...)
	}
	request = append(request, byte(port>>8), byte(port))
	if _, err := conn.Write(request); err != nil {
		return err
	}
	reply := make([]byte, 4)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return err
	}
	if reply[0] != 0x05 {
		return errors.New("SOCKS5 proxy answered with another version")
	}
	if reply[1] != 0x00 {
		return fmt.Errorf("SOCKS5 proxy refused the connection: %d", reply[1])
	}
	var length int
	switch reply[3] {
	case 0x01:
		length = 4
	case 0x04:
		length = 16
	case 0x03:
		size := make([]byte, 1)
		if _, err := io.ReadFull(conn, size); err != nil {
			return err
		}
		length = int(size[0])
	default:
		return errors.New("SOCKS5 proxy answered with an unknown address type")
	}
	discard := make([]byte, length+2)
	_, err = io.ReadFull(conn, discard)
	return err
}

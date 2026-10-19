// Package docker is a small read-only client for the part of the Docker
// Engine API that Raedar uses. It issues only GET requests.
package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// requestTimeout bounds one non-streaming API request.
const requestTimeout = 30 * time.Second

// dialTimeout bounds opening one connection to the daemon.
const dialTimeout = 10 * time.Second

// maxErrorBody is how much of an error response Raedar reads.
const maxErrorBody = 4 << 10

// Client talks to the Docker Engine API over a Unix socket or TCP.
type Client struct {
	requests   *http.Client
	streams    *http.Client
	baseURL    string
	apiVersion string
}

// Connect creates a client for a Docker host address such as
// unix:///var/run/docker.sock or tcp://socket-proxy:2375 and negotiates the
// API version with the daemon.
func Connect(ctx context.Context, host string) (*Client, error) {
	transport, baseURL, err := transport(host)
	if err != nil {
		return nil, err
	}
	client := &Client{
		requests: &http.Client{Transport: transport, Timeout: requestTimeout},
		streams:  &http.Client{Transport: transport},
		baseURL:  baseURL,
	}
	version, err := client.Version(ctx)
	if err != nil {
		return nil, fmt.Errorf("negotiate Docker API version: %w", err)
	}
	if version.APIVersion == "" {
		return nil, errors.New("negotiate Docker API version: the daemon reported no API version")
	}
	client.apiVersion = version.APIVersion
	return client, nil
}

// transport builds the HTTP transport and base URL for a Docker host address.
func transport(host string) (*http.Transport, string, error) {
	parsed, err := url.Parse(host)
	if err != nil {
		return nil, "", fmt.Errorf("parse Docker host: %w", err)
	}
	dialer := &net.Dialer{Timeout: dialTimeout}
	httpTransport := &http.Transport{
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
	}
	switch {
	case strings.EqualFold(parsed.Scheme, "unix") && parsed.Path != "":
		socket := parsed.Path
		httpTransport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socket)
		}
		return httpTransport, "http://docker", nil
	case strings.EqualFold(parsed.Scheme, "tcp") && parsed.Host != "":
		return httpTransport, "http://" + parsed.Host, nil
	}
	return nil, "", errors.New("docker host must be unix://path or tcp://host:port")
}

// Version is the daemon version response.
type Version struct {
	// Version is the daemon version, for example 27.1.1.
	Version string `json:"Version"`
	// APIVersion is the API version Raedar negotiates, for example 1.46.
	APIVersion string `json:"ApiVersion"`
}

// Ping checks that the daemon answers.
func (c *Client) Ping(ctx context.Context) error {
	resp, err := c.get(ctx, "/_ping", nil, false)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// Version returns the daemon version.
func (c *Client) Version(ctx context.Context) (Version, error) {
	var version Version
	if err := c.decode(ctx, "/version", nil, false, &version); err != nil {
		return Version{}, err
	}
	return version, nil
}

// Info is the part of the daemon info response Raedar uses.
type Info struct {
	// NCPU is the number of CPUs the host has.
	NCPU int `json:"NCPU"`
	// MemTotal is the host's total memory in bytes.
	MemTotal int64 `json:"MemTotal"`
}

// Info returns the daemon info, including the host's CPU count and memory.
func (c *Client) Info(ctx context.Context) (Info, error) {
	var info Info
	if err := c.decode(ctx, "/info", nil, true, &info); err != nil {
		return Info{}, err
	}
	return info, nil
}

// get performs one GET request. Streams are not read.
func (c *Client) get(ctx context.Context, path string, query url.Values, stream bool) (*http.Response, error) {
	requestURL := c.baseURL
	if path != "/_ping" && path != "/version" {
		requestURL += "/v" + c.apiVersion
	}
	requestURL += path
	if len(query) > 0 {
		requestURL += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("docker: build request for %s: %w", path, err)
	}
	httpClient := c.requests
	if stream {
		httpClient = c.streams
	}
	resp, err := httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("docker: GET %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, fmt.Errorf("docker: GET %s: %s%s", path, resp.Status, errorMessage(resp.Body))
	}
	return resp, nil
}

// decode performs one GET request and decodes its JSON body into out.
func (c *Client) decode(ctx context.Context, path string, query url.Values, stream bool, out any) error {
	resp, err := c.get(ctx, path, query, stream)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("docker: decode %s: %w", path, err)
	}
	return nil
}

// errorMessage reads the message of a daemon error response.
func errorMessage(body io.Reader) string {
	data, err := io.ReadAll(io.LimitReader(body, maxErrorBody))
	if err != nil {
		return ""
	}
	var response struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &response) != nil || response.Message == "" {
		return ""
	}
	return ": " + response.Message
}

// parseTime parses a Docker timestamp. The zero time means the daemon
// reported no time.
func parseTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || parsed.Year() == 1 {
		return time.Time{}
	}
	return parsed.UTC()
}

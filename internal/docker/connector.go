package docker

import (
	"context"
	"sync"
)

// Connector connects to one Docker API address on first use and keeps the
// client, so that a daemon that was unreachable at startup is used as soon as
// it answers.
type Connector struct {
	address string

	mu     sync.Mutex
	client *Client
}

// NewConnector returns a connector for a Docker host address.
func NewConnector(address string) *Connector {
	return &Connector{address: address}
}

// Client returns the connected client, connecting when needed.
func (c *Connector) Client(ctx context.Context) (*Client, error) {
	c.mu.Lock()
	client := c.client
	c.mu.Unlock()
	if client != nil {
		return client, nil
	}
	client, err := Connect(ctx, c.address)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.client = client
	c.mu.Unlock()
	return client, nil
}

// Stats returns one statistics sample for a container, connecting first when
// needed.
func (c *Connector) Stats(ctx context.Context, id string) (Stats, error) {
	client, err := c.Client(ctx)
	if err != nil {
		return Stats{}, err
	}
	return client.Stats(ctx, id)
}

// Info returns the daemon info, connecting first when needed.
func (c *Connector) Info(ctx context.Context) (Info, error) {
	client, err := c.Client(ctx)
	if err != nil {
		return Info{}, err
	}
	return client.Info(ctx)
}

// DiskUsage reads Docker's own disk usage report, connecting first when
// needed.
func (c *Connector) DiskUsage(ctx context.Context) (DiskUsage, error) {
	client, err := c.Client(ctx)
	if err != nil {
		return DiskUsage{}, err
	}
	return client.DiskUsage(ctx)
}

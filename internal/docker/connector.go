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

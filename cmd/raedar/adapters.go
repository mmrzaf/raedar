package main

import (
	"context"
	"os"

	"github.com/mmrzaf/raedar/internal/docker"
)

// systemBusSocket is where the host's system D-Bus socket is mounted.
const systemBusSocket = "/run/dbus/system_bus_socket"

// dockerAdapter connects the lazily connected Docker client to the interfaces
// the collectors expect.
type dockerAdapter struct {
	connector *docker.Connector
}

// Stats returns one statistics sample for a container.
func (a dockerAdapter) Stats(ctx context.Context, id string) (docker.Stats, error) {
	client, err := a.connector.Client(ctx)
	if err != nil {
		return docker.Stats{}, err
	}
	return client.Stats(ctx, id)
}

// Info returns the daemon info.
func (a dockerAdapter) Info(ctx context.Context) (docker.Info, error) {
	client, err := a.connector.Client(ctx)
	if err != nil {
		return docker.Info{}, err
	}
	return client.Info(ctx)
}

// DiskUsage reads Docker's own disk usage report.
func (a dockerAdapter) DiskUsage(ctx context.Context) (docker.DiskUsage, error) {
	client, err := a.connector.Client(ctx)
	if err != nil {
		return docker.DiskUsage{}, err
	}
	return client.DiskUsage(ctx)
}

// socketExists reports whether a path exists.
func socketExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

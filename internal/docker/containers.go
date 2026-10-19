package docker

import (
	"context"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Container is one container from the container list.
type Container struct {
	// ID is the full container ID.
	ID string
	// Name is the container name without the leading slash.
	Name string
	// ImageRef is the image reference the container was created from.
	ImageRef string
	// ImageID is the ID of the image the container runs.
	ImageID string
	// Labels are the container's labels.
	Labels map[string]string
	// State is created, running, paused, restarting, exited or dead.
	State string
	// Created is when the container was created.
	Created time.Time
}

// Containers lists all containers, including stopped ones.
func (c *Client) Containers(ctx context.Context) ([]Container, error) {
	var listed []struct {
		ID      string            `json:"Id"`
		Names   []string          `json:"Names"`
		Image   string            `json:"Image"`
		ImageID string            `json:"ImageID"`
		Labels  map[string]string `json:"Labels"`
		State   string            `json:"State"`
		Created int64             `json:"Created"`
	}
	if err := c.decode(ctx, "/containers/json", url.Values{"all": {"1"}}, false, &listed); err != nil {
		return nil, err
	}
	containers := make([]Container, 0, len(listed))
	for _, entry := range listed {
		name := ""
		if len(entry.Names) > 0 {
			name = strings.TrimPrefix(entry.Names[0], "/")
		}
		containers = append(containers, Container{
			ID:       entry.ID,
			Name:     name,
			ImageRef: entry.Image,
			ImageID:  entry.ImageID,
			Labels:   entry.Labels,
			State:    entry.State,
			Created:  time.Unix(entry.Created, 0).UTC(),
		})
	}
	return containers, nil
}

// Network is one network a container is attached to.
type Network struct {
	// Name is the network name.
	Name string
	// IPAddress is the container's address on the network.
	IPAddress string
}

// PortBinding is one published port.
type PortBinding struct {
	// HostIP is the address the port is published on.
	HostIP string `json:"HostIp"`
	// HostPort is the published port.
	HostPort string `json:"HostPort"`
}

// ContainerInspect is the part of a container inspect response Raedar uses.
// Container environment variables are not decoded at all.
type ContainerInspect struct {
	// ID is the full container ID.
	ID string
	// Name is the container name without the leading slash.
	Name string
	// ImageRef is the image reference the container was created from.
	ImageRef string
	// ImageID is the ID of the image the container runs.
	ImageID string
	// Labels are the container's labels.
	Labels map[string]string
	// Created is when the container was created.
	Created time.Time
	// StartedAt is when the container last started.
	StartedAt time.Time
	// FinishedAt is when the container last exited.
	FinishedAt time.Time
	// State is created, running, paused, restarting, exited or dead.
	State string
	// Health is starting, healthy or unhealthy, empty without a health check.
	Health string
	// ExitCode is the exit code of the last run.
	ExitCode int
	// OOMKilled reports whether the last run was killed by the OOM killer.
	OOMKilled bool
	// RestartPolicy is no, always, unless-stopped or on-failure.
	RestartPolicy string
	// TTY is whether the container has a TTY, which decides whether the log
	// stream is framed.
	TTY bool
	// Networks are the networks the container is attached to.
	Networks []Network
	// Ports maps container ports such as 443/tcp to their publications.
	Ports map[string][]PortBinding
}

// Inspect returns the details of one container.
func (c *Client) Inspect(ctx context.Context, id string) (ContainerInspect, error) {
	var raw struct {
		ID      string `json:"Id"`
		Name    string `json:"Name"`
		Image   string `json:"Image"`
		Created string `json:"Created"`
		State   struct {
			Status     string `json:"Status"`
			ExitCode   int    `json:"ExitCode"`
			OOMKilled  bool   `json:"OOMKilled"`
			StartedAt  string `json:"StartedAt"`
			FinishedAt string `json:"FinishedAt"`
			Health     *struct {
				Status string `json:"Status"`
			} `json:"Health"`
		} `json:"State"`
		Config struct {
			Image  string            `json:"Image"`
			Labels map[string]string `json:"Labels"`
			Tty    bool              `json:"Tty"`
		} `json:"Config"`
		HostConfig struct {
			RestartPolicy struct {
				Name string `json:"Name"`
			} `json:"RestartPolicy"`
		} `json:"HostConfig"`
		NetworkSettings struct {
			Ports    map[string][]PortBinding `json:"Ports"`
			Networks map[string]struct {
				IPAddress string `json:"IPAddress"`
			} `json:"Networks"`
		} `json:"NetworkSettings"`
	}
	if err := c.decode(ctx, "/containers/"+url.PathEscape(id)+"/json", nil, false, &raw); err != nil {
		return ContainerInspect{}, err
	}
	inspect := ContainerInspect{
		ID:            raw.ID,
		Name:          strings.TrimPrefix(raw.Name, "/"),
		ImageRef:      raw.Config.Image,
		ImageID:       raw.Image,
		Labels:        raw.Config.Labels,
		Created:       parseTime(raw.Created),
		StartedAt:     parseTime(raw.State.StartedAt),
		FinishedAt:    parseTime(raw.State.FinishedAt),
		State:         raw.State.Status,
		ExitCode:      raw.State.ExitCode,
		OOMKilled:     raw.State.OOMKilled,
		RestartPolicy: raw.HostConfig.RestartPolicy.Name,
		TTY:           raw.Config.Tty,
		Ports:         raw.NetworkSettings.Ports,
	}
	if raw.State.Health != nil {
		inspect.Health = raw.State.Health.Status
	}
	networks := make([]Network, 0, len(raw.NetworkSettings.Networks))
	for name, attachment := range raw.NetworkSettings.Networks {
		networks = append(networks, Network{Name: name, IPAddress: attachment.IPAddress})
	}
	sort.Slice(networks, func(i, j int) bool { return networks[i].Name < networks[j].Name })
	inspect.Networks = networks
	return inspect, nil
}

// Stats is one container stats sample.
type Stats struct {
	// Read is the time the daemon took the sample.
	Read time.Time
	// CPUTotalUsage is the total CPU time the container used, in nanoseconds.
	CPUTotalUsage uint64
	// MemoryUsage is the memory the container uses, in bytes.
	MemoryUsage uint64
	// MemoryLimit is the container's memory limit, 0 when there is none.
	MemoryLimit uint64
	// MemoryStats holds the memory sub-counters, such as inactive_file.
	MemoryStats map[string]uint64
	// NetworkRxBytes is the received bytes summed over all interfaces.
	NetworkRxBytes uint64
	// NetworkTxBytes is the sent bytes summed over all interfaces.
	NetworkTxBytes uint64
}

// Stats returns one stats sample for a container.
func (c *Client) Stats(ctx context.Context, id string) (Stats, error) {
	var raw struct {
		Read     string `json:"read"`
		CPUStats struct {
			CPUUsage struct {
				TotalUsage uint64 `json:"total_usage"`
			} `json:"cpu_usage"`
		} `json:"cpu_stats"`
		MemoryStats struct {
			Usage uint64            `json:"usage"`
			Limit uint64            `json:"limit"`
			Stats map[string]uint64 `json:"stats"`
		} `json:"memory_stats"`
		Networks map[string]struct {
			RxBytes uint64 `json:"rx_bytes"`
			TxBytes uint64 `json:"tx_bytes"`
		} `json:"networks"`
	}
	query := url.Values{"stream": {"false"}, "one-shot": {"true"}}
	if err := c.decode(ctx, "/containers/"+url.PathEscape(id)+"/stats", query, false, &raw); err != nil {
		return Stats{}, err
	}
	stats := Stats{
		Read:          parseTime(raw.Read),
		CPUTotalUsage: raw.CPUStats.CPUUsage.TotalUsage,
		MemoryUsage:   raw.MemoryStats.Usage,
		MemoryLimit:   raw.MemoryStats.Limit,
		MemoryStats:   raw.MemoryStats.Stats,
	}
	for _, counters := range raw.Networks {
		stats.NetworkRxBytes += counters.RxBytes
		stats.NetworkTxBytes += counters.TxBytes
	}
	return stats, nil
}

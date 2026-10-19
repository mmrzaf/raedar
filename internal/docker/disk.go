package docker

import "context"

// DiskUsage is Docker's own disk usage.
type DiskUsage struct {
	// ImagesBytes is the size of all images.
	ImagesBytes int64
	// ImagesReclaimableBytes is the size of images no container uses.
	ImagesReclaimableBytes int64
	// BuildCacheBytes is the size of the build cache.
	BuildCacheBytes int64
	// VolumesBytes is the size of all volumes.
	VolumesBytes int64
	// ContainersBytes is the size of the containers' writable layers.
	ContainersBytes int64
}

// DiskUsage reads Docker's disk usage report.
func (c *Client) DiskUsage(ctx context.Context) (DiskUsage, error) {
	var raw struct {
		Images []struct {
			Size       int64 `json:"Size"`
			Containers int64 `json:"Containers"`
		} `json:"Images"`
		BuildCache []struct {
			Size int64 `json:"Size"`
		} `json:"BuildCache"`
		Volumes []struct {
			UsageData struct {
				Size int64 `json:"Size"`
			} `json:"UsageData"`
		} `json:"Volumes"`
		Containers []struct {
			SizeRw int64 `json:"SizeRw"`
		} `json:"Containers"`
	}
	if err := c.decode(ctx, "/system/df", nil, false, &raw); err != nil {
		return DiskUsage{}, err
	}
	var usage DiskUsage
	for _, image := range raw.Images {
		usage.ImagesBytes += image.Size
		if image.Containers == 0 {
			usage.ImagesReclaimableBytes += image.Size
		}
	}
	for _, cache := range raw.BuildCache {
		usage.BuildCacheBytes += cache.Size
	}
	for _, volume := range raw.Volumes {
		usage.VolumesBytes += volume.UsageData.Size
	}
	for _, container := range raw.Containers {
		usage.ContainersBytes += container.SizeRw
	}
	return usage, nil
}

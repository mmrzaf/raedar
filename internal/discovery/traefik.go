package discovery

import (
	"net"
	"sort"
	"strings"
)

// detectTraefik returns the Traefik container: the configured name, a
// container labeled raedar.traefik=true, or a running container whose image
// repository ends in traefik. It returns nil when there is none.
func detectTraefik(configuredName string, containers []*Container, selfHostname string) *Traefik {
	traefik := findTraefikContainer(configuredName, containers)
	if traefik == nil {
		return nil
	}
	return &Traefik{
		Container:    traefik,
		TLSCheckAddr: tlsCheckAddr(traefik, selfContainer(containers, selfHostname)),
	}
}

// findTraefikContainer applies the detection rules in order.
func findTraefikContainer(configuredName string, containers []*Container) *Container {
	if configuredName != "" {
		for _, container := range containers {
			if container.Name == configuredName {
				return container
			}
		}
	}
	for _, container := range containers {
		if isTrue(container.Labels[labelTraefik]) {
			return container
		}
	}
	var candidates []*Container
	for _, container := range containers {
		if container.State == "running" && isTraefikImage(container.ImageRef) {
			candidates = append(candidates, container)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	if len(candidates) == 1 {
		return candidates[0]
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Name < candidates[j].Name })
	for _, candidate := range candidates {
		if publishesPort(candidate, "443") {
			return candidate
		}
	}
	return candidates[0]
}

// selfContainer returns Raedar's own container, which the Docker daemon names
// with the container ID's first characters.
func selfContainer(containers []*Container, hostname string) *Container {
	if hostname == "" {
		return nil
	}
	for _, container := range containers {
		if strings.HasPrefix(container.ID, hostname) {
			return container
		}
	}
	return nil
}

// isTraefikImage reports whether an image reference is a Traefik image.
func isTraefikImage(imageRef string) bool {
	repository := imageRef
	if index := strings.IndexByte(repository, '@'); index >= 0 {
		repository = repository[:index]
	}
	if index := strings.LastIndexByte(repository, ':'); index > strings.LastIndexByte(repository, '/') {
		repository = repository[:index]
	}
	return repository == "traefik" || strings.HasSuffix(repository, "/traefik")
}

// publishesPort reports whether a container publishes a host port.
func publishesPort(container *Container, port string) bool {
	for _, bindings := range container.Ports {
		for _, binding := range bindings {
			if binding.HostPort == port {
				return true
			}
		}
	}
	return false
}

// tlsCheckAddr returns the address where Traefik serves HTTPS: the
// container-side port published as host port 443, reached over a network
// Raedar's own container shares with Traefik. It returns "" when it cannot be
// detected.
func tlsCheckAddr(traefik, self *Container) string {
	if self == nil {
		return ""
	}
	shared := make(map[string]bool, len(self.Networks))
	for _, network := range self.Networks {
		shared[network.Name] = true
	}
	port := publishedContainerPort(traefik, "443")
	if port == "" {
		return ""
	}
	for _, network := range traefik.Networks {
		if !shared[network.Name] || network.IPAddress == "" {
			continue
		}
		return net.JoinHostPort(network.IPAddress, port)
	}
	return ""
}

// publishedContainerPort returns the container port published as the given
// host port, for example 443 for 443/tcp.
func publishedContainerPort(container *Container, hostPort string) string {
	for _, key := range sortedKeys(container.Ports) {
		if !strings.HasSuffix(key, "/tcp") {
			continue
		}
		for _, binding := range container.Ports[key] {
			if binding.HostPort == hostPort {
				return strings.TrimSuffix(key, "/tcp")
			}
		}
	}
	return ""
}

// Package discovery builds the model of the host's apps, slots, containers
// and routes from the Docker API and records deploys and container lifecycle
// events.
package discovery

import (
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/mmrzaf/raedar/internal/config"
	"github.com/mmrzaf/raedar/internal/docker"
)

// Model is a consistent view of the host: its apps, slots, containers and
// routes, and the detected Traefik container. A model is never changed after
// it is published; readers must not modify it.
type Model struct {
	// Apps are the apps by name.
	Apps map[string]*App
	// Routes are the routes by name, which is name@provider.
	Routes map[string]*Route
	// Containers are all containers by Docker ID.
	Containers map[string]*Container
	// Traefik is the detected Traefik container; nil when there is none.
	Traefik *Traefik
}

// App is one app: a group of containers, normally one Compose project.
type App struct {
	// ID is the app's row ID, 0 until it is persisted.
	ID int64
	// Name is the app name.
	Name string
	// Critical reports whether the app's availability incidents are
	// critical.
	Critical bool
	// Alerts reports whether messages are sent for the app's incidents.
	Alerts bool
	// LatencyIgnore are the path prefixes excluded from latency.
	LatencyIgnore []string
	// ErrorsMinRate overrides RAEDAR_ERRORS_MIN_RATE for the app's routes.
	ErrorsMinRate *config.Percent
	// SlowMinP95 overrides RAEDAR_SLOW_MIN_P95 for the app's routes.
	SlowMinP95 *time.Duration
	// Slots are the app's slots by key.
	Slots map[string]*Slot
	// Routes are the app's routes by name.
	Routes map[string]*Route
}

// Slot is one position inside an app. Its history continues across container
// recreations.
type Slot struct {
	// ID is the slot's row ID, 0 until it is persisted.
	ID int64
	// App is the app the slot belongs to.
	App *App
	// Key is the slot key: "service #number" or the container name.
	Key string
	// Service is the Compose service name, or the container name.
	Service string
	// Replica is the Compose container number, 1 for standalone containers.
	Replica int
	// Container is the newest container filling the slot; nil when the slot
	// has none.
	Container *Container
}

// Container is one Docker container.
type Container struct {
	// ID is the Docker container ID.
	ID string
	// Name is the container name.
	Name string
	// ImageRef is the image reference, for example shop/api:2.4.0.
	ImageRef string
	// ImageID is the image ID.
	ImageID string
	// Labels are the container labels.
	Labels map[string]string
	// Identity is where the container's labels say it belongs.
	Identity identity
	// App is the app the container belongs to; nil for containers that are
	// not tracked (ignored and one-off containers).
	App *App
	// Slot is the slot the container fills; nil for containers that are not
	// tracked.
	Slot *Slot
	// State is created, running, paused, restarting, exited or dead.
	State string
	// Health is starting, healthy, unhealthy, or empty without a health
	// check.
	Health string
	// ExitCode is the exit code of the last run.
	ExitCode int
	// OOMKilled reports whether the last run was killed by the OOM killer.
	OOMKilled bool
	// RestartPolicy is no, always, unless-stopped or on-failure.
	RestartPolicy string
	// TTY is the container's Tty setting.
	TTY bool
	// Networks are the networks the container is attached to, sorted by name.
	Networks []docker.Network
	// Ports maps container ports such as 443/tcp to their publications.
	Ports map[string][]docker.PortBinding
	// CreatedAt is when the container was created.
	CreatedAt time.Time
	// StartedAt is when the container last started; the zero time when it
	// never started.
	StartedAt time.Time
}

// Finished reports whether the container is a job that ran to completion: it
// exited with code 0 and is not meant to be restarted, like a migration
// service. Such a container being stopped is not a problem.
func (c *Container) Finished() bool {
	return c.State == "exited" && c.ExitCode == 0 && (c.RestartPolicy == "no" || c.RestartPolicy == "")
}

// Route is one Traefik router.
type Route struct {
	// ID is the route's row ID, 0 until it is persisted.
	ID int64
	// Name is the route name with its provider, for example shop@docker.
	Name string
	// App is the app the route belongs to; nil when the app is unknown.
	App *App
	// Kind is http or tcp.
	Kind string
	// Rule is the router rule text.
	Rule string
	// Domains are the hostnames of the Host and HostSNI matchers.
	Domains []string
	// TLS reports whether the router serves TLS.
	TLS bool
	// Passthrough reports whether a TCP router passes TLS through.
	Passthrough bool
	// Origin is labels or accesslog.
	Origin string
	// Entrypoints are the router's Traefik entrypoints, for display.
	Entrypoints []string
	// Service is the router's Traefik service, for display.
	Service string
}

// Traefik is the detected Traefik container.
type Traefik struct {
	// Container is the Traefik container.
	Container *Container
	// TLSCheckAddr is the host:port where Traefik serves HTTPS; empty when it
	// could not be detected.
	TLSCheckAddr string
}

// The app-level labels, resolved from the app's containers.
const (
	labelApp            = "raedar.app"
	labelIgnore         = "raedar.ignore"
	labelCritical       = "raedar.critical"
	labelAlerts         = "raedar.alerts"
	labelLatencyIgnore  = "raedar.latency.ignore"
	labelErrorsMinRate  = "raedar.errors.min_rate"
	labelSlowMinP95     = "raedar.slow.min_p95"
	labelTraefik        = "raedar.traefik"
	labelComposeProject = "com.docker.compose.project"
	labelComposeService = "com.docker.compose.service"
	labelComposeNumber  = "com.docker.compose.container-number"
	labelComposeOneOff  = "com.docker.compose.oneoff"
	labelTraefikEnable  = "traefik.enable"
)

// UnroutedRoute is the synthetic route for requests that matched no router.
const UnroutedRoute = "(unrouted)"

// build assembles a model from inspected containers.
func build(inspected []docker.ContainerInspect, traefikName, selfHostname string, logger *slog.Logger) *Model {
	model := &Model{
		Apps:       make(map[string]*App),
		Routes:     make(map[string]*Route),
		Containers: make(map[string]*Container, len(inspected)),
	}
	containers := make([]*Container, 0, len(inspected))
	for _, inspect := range inspected {
		container := newContainer(inspect)
		model.Containers[container.ID] = container
		containers = append(containers, container)
	}
	sort.Slice(containers, func(i, j int) bool { return containers[i].ID < containers[j].ID })
	for _, container := range containers {
		if !container.Identity.Tracked() {
			continue
		}
		app := model.app(container.Identity.App)
		slot := app.slot(container.Identity)
		container.App = app
		container.Slot = slot
		if slot.Container == nil || slot.Container.CreatedAt.Before(container.CreatedAt) {
			slot.Container = container
		}
	}
	for _, app := range model.Apps {
		resolveAppLabels(app, containers, logger)
	}
	for _, container := range containers {
		if container.App == nil {
			continue
		}
		for _, route := range routesFromLabels(container.Labels) {
			if existing, ok := model.Routes[route.Name]; ok {
				existing.App = container.App
				continue
			}
			route := route
			route.App = container.App
			model.Routes[route.Name] = &route
			container.App.Routes[route.Name] = &route
		}
	}
	model.Traefik = detectTraefik(traefikName, containers, selfHostname)
	return model
}

// app returns the app with this name, creating it when needed.
func (m *Model) app(name string) *App {
	if app, ok := m.Apps[name]; ok {
		return app
	}
	app := &App{
		Name:   name,
		Alerts: true,
		Slots:  make(map[string]*Slot),
		Routes: make(map[string]*Route),
	}
	m.Apps[name] = app
	return app
}

// slot returns the slot of an identity, creating it when needed.
func (a *App) slot(id identity) *Slot {
	if slot, ok := a.Slots[id.SlotKey]; ok {
		return slot
	}
	slot := &Slot{App: a, Key: id.SlotKey, Service: id.Service, Replica: id.Replica}
	a.Slots[id.SlotKey] = slot
	return slot
}

// newContainer turns one inspect response into a container.
func newContainer(inspect docker.ContainerInspect) *Container {
	return &Container{
		ID:            inspect.ID,
		Name:          inspect.Name,
		ImageRef:      inspect.ImageRef,
		ImageID:       inspect.ImageID,
		Labels:        inspect.Labels,
		Identity:      identify(inspect.Name, inspect.Labels),
		State:         inspect.State,
		Health:        inspect.Health,
		ExitCode:      inspect.ExitCode,
		OOMKilled:     inspect.OOMKilled,
		RestartPolicy: inspect.RestartPolicy,
		TTY:           inspect.TTY,
		Networks:      inspect.Networks,
		Ports:         inspect.Ports,
		CreatedAt:     inspect.Created,
		StartedAt:     inspect.StartedAt,
	}
}

// resolveAppLabels reads the app-level labels from the app's containers. When
// two containers disagree, the most recently created one wins.
func resolveAppLabels(app *App, containers []*Container, logger *slog.Logger) {
	members := make([]*Container, 0, len(containers))
	for _, container := range containers {
		if container.App == app {
			members = append(members, container)
		}
	}
	sort.Slice(members, func(i, j int) bool {
		if !members[i].CreatedAt.Equal(members[j].CreatedAt) {
			return members[i].CreatedAt.After(members[j].CreatedAt)
		}
		return members[i].ID < members[j].ID
	})
	if value, ok := labelValue(members, labelCritical); ok {
		app.Critical = isTrue(value)
	}
	if value, ok := labelValue(members, labelAlerts); ok {
		app.Alerts = !isFalse(value)
	}
	if value, ok := labelValue(members, labelLatencyIgnore); ok {
		app.LatencyIgnore = splitList(value)
	}
	if value, ok := labelValue(members, labelErrorsMinRate); ok {
		minRate, err := config.ParsePercent(value)
		if err != nil {
			logger.Warn("invalid raedar.errors.min_rate label", slog.String("app", app.Name))
		} else {
			app.ErrorsMinRate = &minRate
		}
	}
	if value, ok := labelValue(members, labelSlowMinP95); ok {
		minP95, err := config.ParseDuration(value)
		if err != nil {
			logger.Warn("invalid raedar.slow.min_p95 label", slog.String("app", app.Name))
		} else {
			app.SlowMinP95 = &minP95
		}
	}
}

// labelValue returns the value of a label from the most recently created
// container that sets it.
func labelValue(containers []*Container, label string) (string, bool) {
	for _, container := range containers {
		if value, ok := container.Labels[label]; ok {
			return value, true
		}
	}
	return "", false
}

// isTrue reports whether a label value means true.
func isTrue(value string) bool {
	return strings.EqualFold(strings.TrimSpace(value), "true")
}

// isFalse reports whether a label value means false.
func isFalse(value string) bool {
	return strings.EqualFold(strings.TrimSpace(value), "false")
}

// splitList splits a comma-separated label value.
func splitList(value string) []string {
	var items []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}
	return items
}

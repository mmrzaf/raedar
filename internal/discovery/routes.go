package discovery

import (
	"context"
	"log/slog"
	"sort"

	"github.com/mmrzaf/raedar/internal/store"
)

// RouteLookup is what the access log source needs to know about a route.
type RouteLookup struct {
	// ID is the route's row ID.
	ID int64
	// LatencyIgnore are the path prefixes the route's app excludes from
	// latency.
	LatencyIgnore []string
}

// LookupRoute returns the route for a router name seen in the access log,
// creating it when it is unknown. A name that matches a container's default
// router name belongs to that container's app; every other name, including
// the empty one that becomes (unrouted), belongs to the app that runs
// Traefik.
func (d *Discoverer) LookupRoute(ctx context.Context, routerName string) (RouteLookup, error) {
	name := routerName
	if name == "" {
		name = UnroutedRoute
	}
	d.mu.Lock()
	if route, ok := d.model.Routes[name]; ok && route.ID > 0 {
		d.mu.Unlock()
		return lookupOf(route), nil
	}
	if route, ok := d.accessRoutes[name]; ok {
		d.mu.Unlock()
		return lookupOf(route), nil
	}
	containers := make([]*Container, 0, len(d.model.Containers))
	for _, container := range d.model.Containers {
		containers = append(containers, container)
	}
	traefik := d.model.Traefik
	now := d.clock.Now()
	d.mu.Unlock()

	var app *App
	if matched, ok := defaultRouterApp(containers, name); ok {
		app = matched
	} else if traefik != nil {
		app = traefik.Container.App
	}
	route := &Route{Name: name, App: app, Kind: "http", Origin: "accesslog"}
	id, err := d.store.UpsertRoute(ctx, store.Route{
		Name:   name,
		AppID:  appIDOf(app),
		Kind:   "http",
		Origin: "accesslog",
	}, now)
	if err != nil {
		return RouteLookup{}, err
	}
	route.ID = id
	d.mu.Lock()
	d.accessRoutes[name] = route
	d.mu.Unlock()
	d.logger.Info("route seen in the access log", slog.String("route", name), slog.String("app", appNameOf(app)))
	return lookupOf(route), nil
}

// lookupOf returns the lookup information of a route.
func lookupOf(route *Route) RouteLookup {
	lookup := RouteLookup{ID: route.ID}
	if route.App != nil {
		lookup.LatencyIgnore = route.App.LatencyIgnore
	}
	return lookup
}

// appIDOf returns the row ID of an app, nil when the app is unknown.
func appIDOf(app *App) *int64 {
	if app == nil || app.ID == 0 {
		return nil
	}
	id := app.ID
	return &id
}

// RunningContainer is one running container the collectors sample.
type RunningContainer struct {
	// ID is the Docker container ID.
	ID string
	// SlotID is the slot the container fills.
	SlotID int64
}

// RunningContainers returns the running container of each slot, ordered by
// Docker ID. While a deploy replaces a container, only the newer one is
// returned, so that a slot has one stream of samples.
func (d *Discoverer) RunningContainers() []RunningContainer {
	d.mu.Lock()
	defer d.mu.Unlock()
	containers := make([]RunningContainer, 0, len(d.model.Containers))
	for _, container := range d.model.Containers {
		if container.State != "running" || container.Slot == nil || container.Slot.ID == 0 || container.Slot.Container != container {
			continue
		}
		containers = append(containers, RunningContainer{ID: container.ID, SlotID: container.Slot.ID})
	}
	sort.Slice(containers, func(i, j int) bool { return containers[i].ID < containers[j].ID })
	return containers
}

// TLSDomain is one domain whose certificate is checked.
type TLSDomain struct {
	// Name is the hostname.
	Name string
	// RouteID is a route that serves the domain.
	RouteID int64
}

// TLSDomains returns the domains of the TLS routes, with a route that serves
// each, sorted by domain name. Passthrough routes are not checked, because
// the certificate belongs to the backend.
func (d *Discoverer) TLSDomains() []TLSDomain {
	d.mu.Lock()
	defer d.mu.Unlock()
	byName := make(map[string]int64)
	for _, route := range d.model.Routes {
		if !route.TLS || route.Passthrough || route.ID == 0 {
			continue
		}
		for _, domain := range route.Domains {
			if existing, ok := byName[domain]; !ok || route.ID < existing {
				byName[domain] = route.ID
			}
		}
	}
	domains := make([]TLSDomain, 0, len(byName))
	for _, name := range sortedKeys(byName) {
		domains = append(domains, TLSDomain{Name: name, RouteID: byName[name]})
	}
	return domains
}

// TLSCheckAddr returns the detected address where Traefik serves HTTPS, empty
// when it could not be detected.
func (d *Discoverer) TLSCheckAddr() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.model.Traefik == nil {
		return ""
	}
	return d.model.Traefik.TLSCheckAddr
}

// appNameOf returns the name of an app, empty when the app is unknown.
func appNameOf(app *App) string {
	if app == nil {
		return ""
	}
	return app.Name
}

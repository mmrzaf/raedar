package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// App is one row of the apps table.
type App struct {
	// ID is the app ID.
	ID int64
	// Name is the app name.
	Name string
	// FirstSeen is when the app first had a container.
	FirstSeen time.Time
	// LastSeen is when the app last had a container.
	LastSeen time.Time
	// ArchivedAt is when the app was archived; nil while it is active.
	ArchivedAt *time.Time
}

// Slot is one row of the slots table.
type Slot struct {
	// ID is the slot ID.
	ID int64
	// AppID is the app the slot belongs to.
	AppID int64
	// Key is the slot key: "service #number" or the container name.
	Key string
	// Service is the Compose service name, or the container name.
	Service string
	// Replica is the Compose container number, 1 for standalone containers.
	Replica int
	// FirstSeen is when the slot first had a container.
	FirstSeen time.Time
	// LastSeen is when the slot last had a container.
	LastSeen time.Time
}

// Container is one row of the containers table.
type Container struct {
	// DockerID is the Docker container ID.
	DockerID string
	// SlotID is the slot the container fills.
	SlotID int64
	// Name is the container name.
	Name string
	// ImageRef is the image reference, for example shop/api:2.4.0.
	ImageRef string
	// ImageID is the image ID.
	ImageID string
	// CreatedAt is when the container was created.
	CreatedAt time.Time
	// StartedAt is when the container last started; the zero time when it
	// never started.
	StartedAt time.Time
	// RemovedAt is when the container was removed; nil while it exists.
	RemovedAt *time.Time
}

// Route is one row of the routes table.
type Route struct {
	// ID is the route ID.
	ID int64
	// Name is the route name with its provider, for example shop@docker.
	Name string
	// AppID is the app the route belongs to; nil when the app is unknown.
	AppID *int64
	// Kind is http or tcp.
	Kind string
	// Rule is the Traefik rule text.
	Rule string
	// Domains are the hostnames of the rule.
	Domains []string
	// TLS reports whether the router serves TLS.
	TLS bool
	// Passthrough reports whether a TCP router passes TLS through.
	Passthrough bool
	// Origin is labels or accesslog.
	Origin string
	// FirstSeen is when the route was first seen.
	FirstSeen time.Time
	// LastSeen is when the route was last seen.
	LastSeen time.Time
}

// Model is the persisted model of the host.
type Model struct {
	// Apps are all apps.
	Apps []App
	// Slots are all slots.
	Slots []Slot
	// Containers are all containers, oldest first.
	Containers []Container
	// Routes are all routes.
	Routes []Route
}

// Event is one row of the events table.
type Event struct {
	// ID is the event ID.
	ID int64
	// TS is when the event happened.
	TS time.Time
	// Kind is what happened.
	Kind EventKind
	// Severity is how serious it is.
	Severity Severity
	// AppID, SlotID and RouteID link the event to the model; all may be nil.
	AppID   *int64
	SlotID  *int64
	RouteID *int64
	// Subject is what the event is about.
	Subject string
	// Title is the one-line description.
	Title string
	// Data is the event's payload as JSON.
	Data []byte
}

// LoadModel reads the whole persisted model.
func (s *Store) LoadModel(ctx context.Context) (Model, error) {
	model := Model{}
	apps, err := s.apps(ctx)
	if err != nil {
		return Model{}, err
	}
	model.Apps = apps
	if model.Slots, err = s.slots(ctx); err != nil {
		return Model{}, err
	}
	if model.Containers, err = s.containers(ctx); err != nil {
		return Model{}, err
	}
	if model.Routes, err = s.routes(ctx); err != nil {
		return Model{}, err
	}
	return model, nil
}

// apps reads every app.
func (s *Store) apps(ctx context.Context) ([]App, error) {
	rows, err := s.pool.Query(ctx, `select id, name, first_seen, last_seen, archived_at from apps order by id`)
	if err != nil {
		return nil, fmt.Errorf("read apps: %w", err)
	}
	defer rows.Close()
	var apps []App
	for rows.Next() {
		var app App
		if err := rows.Scan(&app.ID, &app.Name, &app.FirstSeen, &app.LastSeen, &app.ArchivedAt); err != nil {
			return nil, fmt.Errorf("scan app: %w", err)
		}
		apps = append(apps, app)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read apps: %w", err)
	}
	return apps, nil
}

// slots reads every slot.
func (s *Store) slots(ctx context.Context) ([]Slot, error) {
	rows, err := s.pool.Query(ctx, `select id, app_id, key, service, replica, first_seen, last_seen from slots order by id`)
	if err != nil {
		return nil, fmt.Errorf("read slots: %w", err)
	}
	defer rows.Close()
	var slots []Slot
	for rows.Next() {
		var slot Slot
		if err := rows.Scan(&slot.ID, &slot.AppID, &slot.Key, &slot.Service, &slot.Replica, &slot.FirstSeen, &slot.LastSeen); err != nil {
			return nil, fmt.Errorf("scan slot: %w", err)
		}
		slots = append(slots, slot)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read slots: %w", err)
	}
	return slots, nil
}

// containers reads every container, oldest first.
func (s *Store) containers(ctx context.Context) ([]Container, error) {
	rows, err := s.pool.Query(ctx, `select docker_id, slot_id, name, image_ref, image_id, created_at, started_at, removed_at
		from containers order by created_at, docker_id`)
	if err != nil {
		return nil, fmt.Errorf("read containers: %w", err)
	}
	defer rows.Close()
	var containers []Container
	for rows.Next() {
		container, err := scanContainer(rows)
		if err != nil {
			return nil, err
		}
		containers = append(containers, container)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read containers: %w", err)
	}
	return containers, nil
}

// scanContainer reads one containers row in the column order of the queries
// above.
func scanContainer(row interface{ Scan(dest ...any) error }) (Container, error) {
	var (
		container Container
		createdAt *time.Time
		startedAt *time.Time
	)
	if err := row.Scan(&container.DockerID, &container.SlotID, &container.Name, &container.ImageRef,
		&container.ImageID, &createdAt, &startedAt, &container.RemovedAt); err != nil {
		return Container{}, fmt.Errorf("scan container: %w", err)
	}
	if createdAt != nil {
		container.CreatedAt = createdAt.UTC()
	}
	if startedAt != nil {
		container.StartedAt = startedAt.UTC()
	}
	return container, nil
}

// routes reads every route.
func (s *Store) routes(ctx context.Context) ([]Route, error) {
	rows, err := s.pool.Query(ctx, `select id, name, app_id, kind, rule, domains, tls, passthrough, origin, first_seen, last_seen
		from routes order by id`)
	if err != nil {
		return nil, fmt.Errorf("read routes: %w", err)
	}
	defer rows.Close()
	var routes []Route
	for rows.Next() {
		var route Route
		if err := rows.Scan(&route.ID, &route.Name, &route.AppID, &route.Kind, &route.Rule, &route.Domains,
			&route.TLS, &route.Passthrough, &route.Origin, &route.FirstSeen, &route.LastSeen); err != nil {
			return nil, fmt.Errorf("scan route: %w", err)
		}
		routes = append(routes, route)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read routes: %w", err)
	}
	return routes, nil
}

// UpsertApp records an app as seen now and returns its ID. An archived app
// becomes active again.
func (s *Store) UpsertApp(ctx context.Context, name string, at time.Time) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		insert into apps (name, first_seen, last_seen)
		values ($1, $2, $2)
		on conflict (name) do update
		set last_seen = excluded.last_seen, archived_at = null
		returning id`, name, at).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert app %s: %w", name, err)
	}
	return id, nil
}

// UpsertSlot records a slot as seen now and returns its ID.
func (s *Store) UpsertSlot(ctx context.Context, slot Slot, at time.Time) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		insert into slots (app_id, key, service, replica, first_seen, last_seen)
		values ($1, $2, $3, $4, $5, $5)
		on conflict (app_id, key) do update
		set service = excluded.service, replica = excluded.replica, last_seen = excluded.last_seen
		returning id`, slot.AppID, slot.Key, slot.Service, slot.Replica, at).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert slot %s: %w", slot.Key, err)
	}
	return id, nil
}

// UpsertContainer records one container.
func (s *Store) UpsertContainer(ctx context.Context, container Container) error {
	_, err := s.pool.Exec(ctx, `
		insert into containers (docker_id, slot_id, name, image_ref, image_id, created_at, started_at, removed_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8)
		on conflict (docker_id) do update
		set slot_id = excluded.slot_id, name = excluded.name, image_ref = excluded.image_ref,
		    image_id = excluded.image_id, created_at = excluded.created_at,
		    started_at = excluded.started_at, removed_at = excluded.removed_at`,
		container.DockerID, container.SlotID, container.Name, container.ImageRef, container.ImageID,
		nullableTime(container.CreatedAt), nullableTime(container.StartedAt), container.RemovedAt)
	if err != nil {
		return fmt.Errorf("upsert container %s: %w", container.DockerID, err)
	}
	return nil
}

// LatestSlotContainers returns, for each slot, the newest container ever
// recorded in it, including containers that no longer exist. Deploys are
// detected against it, so a container that was removed before its replacement
// appeared still counts as the previous one.
func (s *Store) LatestSlotContainers(ctx context.Context) (map[int64]Container, error) {
	rows, err := s.pool.Query(ctx, `
		select distinct on (slot_id) docker_id, slot_id, name, image_ref, image_id, created_at, started_at, removed_at
		from containers
		order by slot_id, created_at desc nulls last, docker_id`)
	if err != nil {
		return nil, fmt.Errorf("read latest slot containers: %w", err)
	}
	defer rows.Close()
	latest := make(map[int64]Container)
	for rows.Next() {
		container, err := scanContainer(rows)
		if err != nil {
			return nil, err
		}
		latest[container.SlotID] = container
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read latest slot containers: %w", err)
	}
	return latest, nil
}

// MarkContainersRemoved records that containers no longer exist.
func (s *Store) MarkContainersRemoved(ctx context.Context, dockerIDs []string, at time.Time) error {
	if len(dockerIDs) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx,
		`update containers set removed_at = $2 where docker_id = any($1) and removed_at is null`,
		dockerIDs, at)
	if err != nil {
		return fmt.Errorf("mark containers removed: %w", err)
	}
	return nil
}

// UpsertRoute records a route as seen now and returns its ID.
func (s *Store) UpsertRoute(ctx context.Context, route Route, at time.Time) (int64, error) {
	domains := route.Domains
	if domains == nil {
		domains = []string{}
	}
	var id int64
	err := s.pool.QueryRow(ctx, `
		insert into routes (name, app_id, kind, rule, domains, tls, passthrough, origin, first_seen, last_seen)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9)
		on conflict (name) do update
		set app_id = excluded.app_id, kind = excluded.kind, rule = excluded.rule, domains = excluded.domains,
		    tls = excluded.tls, passthrough = excluded.passthrough, origin = excluded.origin,
		    last_seen = excluded.last_seen
		returning id`,
		route.Name, route.AppID, route.Kind, route.Rule, domains, route.TLS, route.Passthrough,
		route.Origin, at).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert route %s: %w", route.Name, err)
	}
	return id, nil
}

// ArchiveStaleApps archives the apps whose last container was seen before
// cutoff and returns their names.
func (s *Store) ArchiveStaleApps(ctx context.Context, cutoff, at time.Time) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		update apps set archived_at = $2
		where archived_at is null and last_seen < $1
		returning name`, cutoff, at)
	if err != nil {
		return nil, fmt.Errorf("archive apps: %w", err)
	}
	defer rows.Close()
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("archive apps: %w", err)
	}
	return names, nil
}

// AddEvent records one event and returns its ID.
func (s *Store) AddEvent(ctx context.Context, event Event) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		insert into events (ts, kind, severity, app_id, slot_id, route_id, subject, title, data)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		returning id`,
		event.TS, event.Kind, event.Severity, event.AppID, event.SlotID, event.RouteID,
		event.Subject, event.Title, event.Data).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("add %s event: %w", event.Kind, err)
	}
	return id, nil
}

// RecentEvent returns the newest event of one kind for an app at or after
// since. It returns nil when there is none.
func (s *Store) RecentEvent(ctx context.Context, appID int64, kind EventKind, since time.Time) (*Event, error) {
	var event Event
	err := s.pool.QueryRow(ctx, `
		select id, ts, kind, severity, app_id, slot_id, route_id, subject, title, data
		from events
		where app_id = $1 and kind = $2 and ts >= $3
		order by ts desc, id desc
		limit 1`, appID, kind, since).Scan(&event.ID, &event.TS, &event.Kind, &event.Severity,
		&event.AppID, &event.SlotID, &event.RouteID, &event.Subject, &event.Title, &event.Data)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read recent %s event: %w", kind, err)
	}
	return &event, nil
}

// UpdateEvent replaces the title and payload of one event.
func (s *Store) UpdateEvent(ctx context.Context, id int64, title string, data []byte) error {
	_, err := s.pool.Exec(ctx, `update events set title = $2, data = $3 where id = $1`, id, title, data)
	if err != nil {
		return fmt.Errorf("update event %d: %w", id, err)
	}
	return nil
}

// RecordSourceStatus records the state of one source.
func (s *Store) RecordSourceStatus(ctx context.Context, source, status, lastError string, at time.Time) error {
	_, err := s.pool.Exec(ctx, `
		insert into source_state (source, status, last_error, updated_at)
		values ($1, $2, $3, $4)
		on conflict (source) do update
		set status = excluded.status, last_error = excluded.last_error, updated_at = excluded.updated_at`,
		source, status, nullableString(lastError), at)
	if err != nil {
		return fmt.Errorf("record %s source status: %w", source, err)
	}
	return nil
}

// nullableTime returns nil for the zero time.
func nullableTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// nullableString returns nil for an empty string.
func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

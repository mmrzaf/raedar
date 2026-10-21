package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/mmrzaf/raedar/internal/backoff"
	"github.com/mmrzaf/raedar/internal/clock"
	"github.com/mmrzaf/raedar/internal/docker"
	"github.com/mmrzaf/raedar/internal/sourcestatus"
	"github.com/mmrzaf/raedar/internal/store"
)

// SourceName names the Docker API in the source state.
const SourceName = "docker"

const (
	// resyncInterval is how often the whole model is rebuilt.
	resyncInterval = 5 * time.Minute
	// retryBase and retryMax bound the delay between attempts to reach the
	// Docker API and between reconnects of its event stream.
	retryBase = time.Second
	retryMax  = 30 * time.Second
	// resyncDebounce coalesces a burst of container events into one resync.
	resyncDebounce = 300 * time.Millisecond
	// pendingFlushInterval is how often outstanding crash classifications are
	// completed.
	pendingFlushInterval = time.Second
	// deployMergeWindow is how long deploys of one app are merged into a
	// single event.
	deployMergeWindow = 2 * time.Minute
	// appArchiveAfter is how long an app without containers stays active.
	appArchiveAfter = 7 * 24 * time.Hour
	// eventBuffer is how many daemon events wait to be processed.
	eventBuffer = 1024
)

// Discoverer keeps the model of apps, slots, containers and routes up to date
// from the Docker API and records deploys and container lifecycle events.
type Discoverer struct {
	host        string
	traefikName string
	self        string
	store       *store.Store
	logger      *slog.Logger
	status      *sourcestatus.Reporter
	clock       clock.Clock
	resyncEvery time.Duration

	mu           sync.Mutex
	connected    *docker.Client
	model        *Model
	accessRoutes map[string]*Route

	classifier *classifier

	// deploySilence, when > 0, creates an app-scoped silence after every
	// deploy so a restart does not page. Set through SetDeploySilence.
	deploySilence time.Duration
}

// SetDeploySilence configures the auto-deploy silence window. Zero
// disables it.
func (d *Discoverer) SetDeploySilence(window time.Duration) {
	d.deploySilence = window
}

// New returns a discoverer for the Docker API at host, which is a unix:// or
// tcp:// address. traefikName is the configured Traefik container name, empty
// for detection. self is the process host name, used to find Raedar's own
// container among the containers the daemon reports.
func New(host, traefikName, self string, st *store.Store, logger *slog.Logger, clock clock.Clock) *Discoverer {
	return &Discoverer{
		host:        host,
		traefikName: traefikName,
		self:        self,
		store:       st,
		logger:      logger,
		clock:       clock,
		resyncEvery: resyncInterval,
		classifier:  newClassifier(),
		model: &Model{
			Apps:       make(map[string]*App),
			Routes:     make(map[string]*Route),
			Containers: make(map[string]*Container),
		},
		accessRoutes: make(map[string]*Route),
		status:       sourcestatus.New(SourceName, st, clock, logger),
	}
}

// Run keeps the model up to date until ctx is done: it connects to the Docker
// API, retrying while it is unreachable, runs a full discovery pass, and then
// follows the Docker event stream with a full resync every resyncEvery and one
// after container events. Load must have been called first, so that routes and
// apps are known from the first request.
func (d *Discoverer) Run(ctx context.Context) error {
	retries := backoff.New(retryBase, retryMax)
	for ctx.Err() == nil {
		client, err := d.client(ctx)
		if err != nil {
			d.status.Report(ctx, store.StatusUnavailable, err)
			d.logger.Warn("Docker API not reachable", slog.String("error", err.Error()))
			select {
			case <-ctx.Done():
				return nil
			case <-d.clock.After(retries.Next()):
			}
			continue
		}
		retries.Reset()
		d.run(ctx, client)
	}
	return nil
}

// client returns the connected Docker client, connecting when needed.
func (d *Discoverer) client(ctx context.Context) (*docker.Client, error) {
	d.mu.Lock()
	client := d.connected
	d.mu.Unlock()
	if client != nil {
		return client, nil
	}
	client, err := docker.Connect(ctx, d.host)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.connected = client
	d.mu.Unlock()
	return client, nil
}

// run follows the Docker event stream and resyncs until ctx is done.
func (d *Discoverer) run(ctx context.Context, client *docker.Client) {
	if err := d.Resync(ctx); err != nil {
		d.logger.Warn("initial Docker discovery failed", slog.String("error", err.Error()))
	}
	events := make(chan docker.Event, eventBuffer)
	go d.watchEvents(ctx, client, events)
	resync := make(chan struct{}, 1)
	ticker := time.NewTicker(d.resyncEvery)
	defer ticker.Stop()
	flush := time.NewTicker(pendingFlushInterval)
	defer flush.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-events:
			d.handleEvent(ctx, event, resync)
		case <-resync:
			d.coalesce(ctx, resync)
			d.resyncAndLog(ctx)
		case <-flush.C:
			d.flushPending(ctx)
		case <-ticker.C:
			d.resyncAndLog(ctx)
		}
	}
}

// Load reads the persisted model into memory, so that a resync recognizes
// deploys that happened while Raedar was down.
func (d *Discoverer) Load(ctx context.Context) error {
	persisted, err := d.store.LoadModel(ctx)
	if err != nil {
		return err
	}
	model := &Model{
		Apps:       make(map[string]*App),
		Routes:     make(map[string]*Route),
		Containers: make(map[string]*Container),
	}
	appsByID := make(map[int64]*App, len(persisted.Apps))
	for _, app := range persisted.Apps {
		loaded := &App{
			ID:     app.ID,
			Name:   app.Name,
			Alerts: true,
			Slots:  make(map[string]*Slot),
			Routes: make(map[string]*Route),
		}
		model.Apps[app.Name] = loaded
		appsByID[app.ID] = loaded
	}
	slotsByID := make(map[int64]*Slot, len(persisted.Slots))
	for _, slot := range persisted.Slots {
		app := appsByID[slot.AppID]
		if app == nil {
			continue
		}
		loaded := &Slot{ID: slot.ID, App: app, Key: slot.Key, Service: slot.Service, Replica: slot.Replica}
		app.Slots[slot.Key] = loaded
		slotsByID[slot.ID] = loaded
	}
	for _, container := range persisted.Containers {
		slot := slotsByID[container.SlotID]
		if slot == nil {
			continue
		}
		loaded := &Container{
			ID:        container.DockerID,
			Name:      container.Name,
			ImageRef:  container.ImageRef,
			ImageID:   container.ImageID,
			CreatedAt: container.CreatedAt,
			Slot:      slot,
			App:       slot.App,
		}
		model.Containers[loaded.ID] = loaded
		if slot.Container == nil || slot.Container.CreatedAt.Before(loaded.CreatedAt) {
			slot.Container = loaded
		}
	}
	for _, route := range persisted.Routes {
		loaded := &Route{
			ID:          route.ID,
			Name:        route.Name,
			Kind:        route.Kind,
			Rule:        route.Rule,
			Domains:     route.Domains,
			TLS:         route.TLS,
			Passthrough: route.Passthrough,
			Origin:      route.Origin,
		}
		if route.AppID != nil {
			loaded.App = appsByID[*route.AppID]
		}
		if loaded.App != nil {
			loaded.App.Routes[route.Name] = loaded
		}
		model.Routes[route.Name] = loaded
	}
	d.publish(model)
	return nil
}

// Model returns the current model. The caller must not modify it.
func (d *Discoverer) Model() *Model {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.model
}

// Healthy reports whether the Docker source last worked.
func (d *Discoverer) Healthy() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.status.Status() == store.StatusOK
}

// TraefikContainer returns the detected Traefik container, nil when there is
// none.
func (d *Discoverer) TraefikContainer() *Container {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.model.Traefik == nil {
		return nil
	}
	return d.model.Traefik.Container
}

// Resync rebuilds the model from the Docker API and persists it.
func (d *Discoverer) Resync(ctx context.Context) error {
	client, err := d.client(ctx)
	if err != nil {
		d.status.Report(ctx, store.StatusUnavailable, err)
		return fmt.Errorf("connect to the Docker API: %w", err)
	}
	listed, err := client.Containers(ctx)
	if err != nil {
		d.status.Report(ctx, store.StatusUnavailable, err)
		return fmt.Errorf("list containers: %w", err)
	}
	inspected := make([]docker.ContainerInspect, 0, len(listed))
	failures := 0
	for _, container := range listed {
		inspect, err := client.Inspect(ctx, container.ID)
		if err != nil {
			failures++
			d.logger.Warn("inspect container",
				slog.String("container", container.Name), slog.String("error", err.Error()))
			continue
		}
		inspected = append(inspected, inspect)
	}
	previous := d.Model()
	model := build(inspected, d.traefikName, d.self, d.logger)
	// Routes first seen in the access log are not part of the Docker model;
	// keep them alongside it.
	d.mu.Lock()
	for name, route := range d.accessRoutes {
		if _, ok := model.Routes[name]; !ok {
			model.Routes[name] = route
		}
	}
	d.mu.Unlock()
	for name, route := range previous.Routes {
		if route.Origin != "accesslog" {
			continue
		}
		if _, ok := model.Routes[name]; !ok {
			model.Routes[name] = route
		}
	}
	if err := d.persist(ctx, model, previous); err != nil {
		d.status.Report(ctx, store.StatusUnavailable, err)
		return err
	}
	d.publish(model)
	if failures > 0 {
		d.status.Report(ctx, store.StatusDegraded,
			fmt.Errorf("%d of %d containers could not be inspected", failures, len(listed)))
		return nil
	}
	d.status.Report(ctx, store.StatusOK, nil)
	return nil
}

// persist writes the model through the store and records the deploys and
// disappearances it implies.
func (d *Discoverer) persist(ctx context.Context, model, previous *Model) error {
	now := d.clock.Now()
	for _, name := range sortedKeys(model.Apps) {
		app := model.Apps[name]
		id, err := d.store.UpsertApp(ctx, app.Name, now)
		if err != nil {
			return err
		}
		app.ID = id
		for _, key := range sortedKeys(app.Slots) {
			slot := app.Slots[key]
			slotID, err := d.store.UpsertSlot(ctx, store.Slot{
				AppID:   id,
				Key:     slot.Key,
				Service: slot.Service,
				Replica: slot.Replica,
			}, now)
			if err != nil {
				return err
			}
			slot.ID = slotID
		}
	}
	// Deploys are detected against what the database remembers of each slot,
	// not against the previous model, which forgets a container once it is
	// removed. Read it before this pass writes the new containers.
	latest, err := d.store.LatestSlotContainers(ctx)
	if err != nil {
		return err
	}
	seen := make(map[string]bool, len(model.Containers))
	for _, id := range sortedKeys(model.Containers) {
		container := model.Containers[id]
		seen[id] = true
		if container.Slot == nil || container.Slot.ID == 0 {
			continue
		}
		if err := d.store.UpsertContainer(ctx, store.Container{
			DockerID:  container.ID,
			SlotID:    container.Slot.ID,
			Name:      container.Name,
			ImageRef:  container.ImageRef,
			ImageID:   container.ImageID,
			CreatedAt: container.CreatedAt,
			StartedAt: container.StartedAt,
		}); err != nil {
			return err
		}
	}
	var gone []string
	for id := range previous.Containers {
		if !seen[id] {
			gone = append(gone, id)
		}
	}
	if err := d.store.MarkContainersRemoved(ctx, gone, now); err != nil {
		return err
	}
	for _, name := range sortedKeys(model.Routes) {
		route := model.Routes[name]
		var appID *int64
		if route.App != nil && route.App.ID > 0 {
			appID = &route.App.ID
		}
		id, err := d.store.UpsertRoute(ctx, store.Route{
			Name:        route.Name,
			AppID:       appID,
			Kind:        route.Kind,
			Rule:        route.Rule,
			Domains:     route.Domains,
			TLS:         route.TLS,
			Passthrough: route.Passthrough,
			Origin:      route.Origin,
		}, now)
		if err != nil {
			return err
		}
		route.ID = id
	}
	if err := d.recordChanges(ctx, latest, model, now); err != nil {
		return err
	}
	archived, err := d.store.ArchiveStaleApps(ctx, now.Add(-appArchiveAfter), now)
	if err != nil {
		return err
	}
	for _, name := range archived {
		d.logger.Info("archived app", slog.String("app", name))
	}
	return nil
}

// recordChanges records the deploy and recreated events a resync implies: a
// new container in a slot whose image differs from the slot's previous
// container is a deploy, the same image is a recreation. latest holds the
// previous container of each slot.
func (d *Discoverer) recordChanges(ctx context.Context, latest map[int64]store.Container, model *Model, now time.Time) error {
	for _, appName := range sortedKeys(model.Apps) {
		app := model.Apps[appName]
		for _, key := range sortedKeys(app.Slots) {
			slot := app.Slots[key]
			container := slot.Container
			if container == nil {
				continue
			}
			previous, ok := latest[slot.ID]
			if !ok || previous.DockerID == container.ID || container.CreatedAt.Before(previous.CreatedAt) {
				continue
			}
			if previous.ImageID == container.ImageID {
				if err := d.recordRecreated(ctx, app, slot, container, now); err != nil {
					return err
				}
				continue
			}
			old := &Container{ID: previous.DockerID, Name: previous.Name, ImageRef: previous.ImageRef, ImageID: previous.ImageID}
			if err := d.recordDeploy(ctx, app, slot, old, container, now); err != nil {
				return err
			}
		}
	}
	return nil
}

// deployChange is one slot's image change in a deploy event.
type deployChange struct {
	// Slot is the slot key.
	Slot string `json:"slot"`
	// Service is the Compose service name.
	Service string `json:"service"`
	// OldImage and NewImage are the image references before and after.
	OldImage string `json:"old_image"`
	NewImage string `json:"new_image"`
	// OldImageID and NewImageID are the short image IDs before and after.
	OldImageID string `json:"old_image_id"`
	NewImageID string `json:"new_image_id"`
}

// deployData is the payload of a deploy event.
type deployData struct {
	// Services are the Compose services involved.
	Services []string `json:"services"`
	// Changes are the individual slot changes.
	Changes []deployChange `json:"changes"`
}

// recordDeploy records a deploy, merging deploys of the same app within
// deployMergeWindow into one event.
func (d *Discoverer) recordDeploy(ctx context.Context, app *App, slot *Slot, old *Container, container *Container, now time.Time) error {
	change := deployChange{
		Slot:       slot.Key,
		Service:    slot.Service,
		OldImage:   old.ImageRef,
		NewImage:   container.ImageRef,
		OldImageID: shortImageID(old.ImageID),
		NewImageID: shortImageID(container.ImageID),
	}
	at := container.CreatedAt
	if at.IsZero() {
		at = now
	}
	if app.ID > 0 {
		recent, err := d.store.RecentEvent(ctx, app.ID, store.EventDeploy, at.Add(-deployMergeWindow))
		if err != nil {
			return err
		}
		if recent != nil {
			var merged deployData
			if err := json.Unmarshal(recent.Data, &merged); err != nil {
				return fmt.Errorf("decode deploy event %d: %w", recent.ID, err)
			}
			merged.Services = appendUnique(merged.Services, change.Service)
			merged.Changes = append(merged.Changes, change)
			return d.updateEvent(ctx, recent.ID, deployTitle(merged), merged)
		}
	}
	appID := app.ID
	slotID := slot.ID
	event := store.Event{
		TS:       at,
		Kind:     store.EventDeploy,
		Severity: store.SeverityInfo,
		Subject:  app.Name,
		Title:    deployTitle(deployData{Services: []string{slot.Service}, Changes: []deployChange{change}}),
	}
	if appID > 0 {
		event.AppID = &appID
	}
	if slotID > 0 {
		event.SlotID = &slotID
	}
	data, err := json.Marshal(deployData{Services: []string{slot.Service}, Changes: []deployChange{change}})
	if err != nil {
		return fmt.Errorf("encode deploy event: %w", err)
	}
	event.Data = data
	if _, err := d.store.AddEvent(ctx, event); err != nil {
		return err
	}
	d.logger.Info("deploy", slog.String("app", app.Name), slog.String("slot", slot.Key),
		slog.String("image", container.ImageRef))
	d.silenceDeploy(ctx, app.ID, app.Name, container.ImageRef, at)
	return nil
}

// silenceDeploy writes an app-scoped silence for the deploy just
// recorded. It is idempotent per (app, image): re-recording the same
// deploy does not extend or duplicate the silence.
func (d *Discoverer) silenceDeploy(ctx context.Context, appID int64, appName, image string, at time.Time) {
	if d.deploySilence <= 0 || appID <= 0 {
		return
	}
	id := appID
	endsAt := at.Add(d.deploySilence)
	now := d.clock.Now()
	if endsAt.Before(now) {
		endsAt = now.Add(d.deploySilence)
	}
	key := fmt.Sprintf("deploy:%d:%s", appID, image)
	reason := fmt.Sprintf("deploy %s", image)
	if _, err := d.store.CreateSilence(ctx, store.Silence{
		Kind:      store.SilenceAutoDeploy,
		AppID:     &id,
		Reason:    reason,
		StartsAt:  now,
		EndsAt:    endsAt,
		CreatedBy: strPtr("auto:deploy"),
		DedupKey:  &key,
	}); err != nil {
		d.logger.Warn("record auto-deploy silence", slog.String("app", appName), slog.String("error", err.Error()))
	}
}

// strPtr returns a pointer to s.
func strPtr(s string) *string { return &s }

// updateEvent replaces the title and payload of an event.
func (d *Discoverer) updateEvent(ctx context.Context, id int64, title string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode event %d: %w", id, err)
	}
	return d.store.UpdateEvent(ctx, id, title, data)
}

// deployTitle returns the title of a deploy event.
func deployTitle(data deployData) string {
	if len(data.Changes) == 1 {
		return fmt.Sprintf("deploy: %s → %s", data.Changes[0].OldImage, data.Changes[0].NewImage)
	}
	return fmt.Sprintf("deploy: %d slots", len(data.Changes))
}

// recordRecreated records that a slot's container was recreated from the same
// image.
func (d *Discoverer) recordRecreated(ctx context.Context, app *App, slot *Slot, container *Container, now time.Time) error {
	at := container.CreatedAt
	if at.IsZero() {
		at = now
	}
	payload := struct {
		Image   string `json:"image"`
		ImageID string `json:"image_id"`
	}{Image: container.ImageRef, ImageID: shortImageID(container.ImageID)}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode recreated event: %w", err)
	}
	event := store.Event{
		TS:       at,
		Kind:     store.EventRecreated,
		Severity: store.SeverityInfo,
		Subject:  app.Name,
		Title:    "recreated: " + container.ImageRef,
		Data:     data,
	}
	if app.ID > 0 {
		event.AppID = &app.ID
	}
	if slot.ID > 0 {
		event.SlotID = &slot.ID
	}
	if _, err := d.store.AddEvent(ctx, event); err != nil {
		return err
	}
	d.logger.Info("recreated", slog.String("app", app.Name), slog.String("slot", slot.Key))
	return nil
}

// watchEvents follows the Docker event stream and passes every event to the
// loop, reconnecting after a stream ends.
func (d *Discoverer) watchEvents(ctx context.Context, client *docker.Client, events chan<- docker.Event) {
	retries := backoff.New(retryBase, retryMax)
	for ctx.Err() == nil {
		started := d.clock.Now()
		err := client.Events(ctx, func(event docker.Event) {
			select {
			case events <- event:
			case <-ctx.Done():
			}
		})
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			d.logger.Warn("Docker event stream ended", slog.String("error", err.Error()))
		}
		if d.clock.Now().Sub(started) > retryMax {
			retries.Reset() // the stream worked for a while before it ended
		}
		select {
		case <-ctx.Done():
			return
		case <-d.clock.After(retries.Next()):
		}
	}
}

// handleEvent turns one Docker event into lifecycle events and asks for a
// resync when the set of containers may have changed.
func (d *Discoverer) handleEvent(ctx context.Context, event docker.Event, resync chan<- struct{}) {
	for _, lifecycle := range d.classifier.Feed(event) {
		d.recordLifecycle(ctx, lifecycle)
	}
	switch event.Action {
	case "create", "start", "die", "destroy", "rename", "restart":
		select {
		case resync <- struct{}{}:
		default:
		}
	}
}

// coalesce waits until a burst of resync requests has quieted down.
func (d *Discoverer) coalesce(ctx context.Context, resync <-chan struct{}) {
	for {
		select {
		case <-resync:
		case <-d.clock.After(resyncDebounce):
			return
		case <-ctx.Done():
			return
		}
	}
}

// flushPending records the die events whose grace period passed.
func (d *Discoverer) flushPending(ctx context.Context) {
	for _, lifecycle := range d.classifier.Flush(d.clock.Now()) {
		d.recordLifecycle(ctx, lifecycle)
	}
}

// resyncAndLog rebuilds the model, logging a failure without stopping.
func (d *Discoverer) resyncAndLog(ctx context.Context) {
	if err := d.Resync(ctx); err != nil {
		d.logger.Warn("Docker discovery failed", slog.String("error", err.Error()))
	}
}

// lifecycleTarget returns the app and slot a lifecycle event belongs to. It
// finds the container in the model by ID and otherwise places it by its
// labels, so a container that appeared after the last resync is still
// attributed to its slot. ok is false for containers the model does not track:
// ignored and one-off containers, and short-lived containers of tools that
// were never part of an app.
func (d *Discoverer) lifecycleTarget(lifecycle LifecycleEvent) (*App, *Slot, bool) {
	model := d.Model()
	if container, ok := model.Containers[lifecycle.ContainerID]; ok {
		return container.App, container.Slot, container.Slot != nil
	}
	id := identify(lifecycle.Name, lifecycle.Labels)
	if !id.Tracked() {
		return nil, nil, false
	}
	app := model.Apps[id.App]
	if app == nil {
		return nil, nil, false
	}
	slot := app.Slots[id.SlotKey]
	return app, slot, slot != nil
}

// recordLifecycle stores one derived lifecycle event for the app and slot of
// its container. Events of containers outside the model are dropped.
func (d *Discoverer) recordLifecycle(ctx context.Context, lifecycle LifecycleEvent) {
	app, slot, ok := d.lifecycleTarget(lifecycle)
	if !ok {
		return
	}
	event := store.Event{
		TS:       lifecycle.Time,
		Kind:     lifecycle.Kind,
		Severity: store.SeverityInfo,
		Subject:  slot.Key,
		Title:    lifecycleTitle(lifecycle),
	}
	if app.ID > 0 {
		event.AppID = &app.ID
	}
	if slot.ID > 0 {
		event.SlotID = &slot.ID
	}
	payload := struct {
		Image    string `json:"image"`
		ExitCode *int   `json:"exit_code,omitempty"`
		OOM      *bool  `json:"oom,omitempty"`
		Health   string `json:"health,omitempty"`
	}{Image: lifecycle.ImageRef}
	switch lifecycle.Kind {
	case store.EventCrashed:
		exitCode := lifecycle.ExitCode
		oom := lifecycle.OOMKilled
		payload.ExitCode = &exitCode
		payload.OOM = &oom
		event.Severity = store.SeverityWarning
	case store.EventHealth:
		payload.Health = lifecycle.Health
	}
	data, err := json.Marshal(payload)
	if err != nil {
		d.logger.Warn("encode container event", slog.String("error", err.Error()))
		return
	}
	event.Data = data
	if _, err := d.store.AddEvent(ctx, event); err != nil {
		d.logger.Warn("record container event", slog.String("kind", string(lifecycle.Kind)), slog.String("error", err.Error()))
	}
}

// lifecycleTitle returns the one-line title of a lifecycle event.
func lifecycleTitle(lifecycle LifecycleEvent) string {
	switch lifecycle.Kind {
	case store.EventCrashed:
		if lifecycle.OOMKilled {
			return fmt.Sprintf("crashed: exit code %d, out of memory", lifecycle.ExitCode)
		}
		return fmt.Sprintf("crashed: exit code %d", lifecycle.ExitCode)
	case store.EventHealth:
		return "health: " + lifecycle.Health
	default:
		return string(lifecycle.Kind)
	}
}

// shortImageID returns the twelve characters Docker shows for an image ID.
func shortImageID(imageID string) string {
	id := imageID
	for _, prefix := range []string{"sha256:", "sha512:"} {
		if trimmed, ok := strings.CutPrefix(id, prefix); ok {
			id = trimmed
		}
	}
	if len(id) > 12 {
		id = id[:12]
	}
	return id
}

// appendUnique appends value unless it is already in values.
func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

// publish makes the model current.
func (d *Discoverer) publish(model *Model) {
	d.mu.Lock()
	d.model = model
	d.mu.Unlock()
}

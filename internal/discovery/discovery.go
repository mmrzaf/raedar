package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/mmrzaf/raedar/internal/docker"
	"github.com/mmrzaf/raedar/internal/store"
)

// SourceName names the Docker API in the source state.
const SourceName = "docker"

const (
	// resyncInterval is how often the whole model is rebuilt.
	resyncInterval = 5 * time.Minute
	// connectBase and connectMax bound the delay between attempts to reach
	// the Docker API.
	connectBase = time.Second
	connectMax  = 30 * time.Second
	// resyncDebounce coalesces a burst of container events into one resync.
	resyncDebounce = 300 * time.Millisecond
	// pendingFlushInterval is how often outstanding crash classifications are
	// completed.
	pendingFlushInterval = time.Second
	// streamRetryBase and streamRetryMax bound the reconnect delay of the
	// Docker event stream.
	streamRetryBase = time.Second
	streamRetryMax  = 30 * time.Second
	// deployMergeWindow is how long deploys of one app are merged into a
	// single event.
	deployMergeWindow = 2 * time.Minute
	// appArchiveAfter is how long an app without containers stays active.
	appArchiveAfter = 7 * 24 * time.Hour
	// eventBuffer is how many daemon events wait to be processed.
	eventBuffer = 1024
)

// Clock provides the time to the discovery loop, so that tests run without
// waiting.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// After returns a channel that receives the current time after d.
	After(d time.Duration) <-chan time.Time
}

// SystemClock returns the clock used outside tests.
func SystemClock() Clock {
	return systemClock{}
}

// systemClock is the clock of the running process.
type systemClock struct{}

// Now returns the current time in UTC.
func (systemClock) Now() time.Time {
	return time.Now().UTC()
}

// After returns a channel that receives the current time after d.
func (systemClock) After(d time.Duration) <-chan time.Time {
	return time.After(d)
}

// Discoverer keeps the model of apps, slots, containers and routes up to date
// from the Docker API and records deploys and container lifecycle events.
type Discoverer struct {
	host        string
	traefikName string
	self        string
	store       *store.Store
	logger      *slog.Logger
	clock       Clock
	resyncEvery time.Duration
	connectBase time.Duration

	mu           sync.Mutex
	connected    *docker.Client
	model        *Model
	accessRoutes map[string]*Route
	status       string
	statusError  string

	classifier *classifier
}

// New returns a discoverer for the Docker API at host, which is a unix:// or
// tcp:// address. traefikName is the configured Traefik container name, empty
// for detection. self is the process host name, used to find Raedar's own
// container among the containers the daemon reports.
func New(host, traefikName, self string, st *store.Store, logger *slog.Logger, clock Clock) *Discoverer {
	return &Discoverer{
		host:        host,
		traefikName: traefikName,
		self:        self,
		store:       st,
		logger:      logger,
		clock:       clock,
		resyncEvery: resyncInterval,
		connectBase: connectBase,
		classifier:  newClassifier(),
		model: &Model{
			Apps:       make(map[string]*App),
			Routes:     make(map[string]*Route),
			Containers: make(map[string]*Container),
		},
		accessRoutes: make(map[string]*Route),
		status:       store.StatusUnavailable,
	}
}

// Run loads the persisted model and keeps the model up to date until ctx is
// done: it connects to the Docker API, retrying while it is unreachable, runs
// a full discovery pass, and then follows the Docker event stream with a full
// resync every resyncEvery and one after container events.
func (d *Discoverer) Run(ctx context.Context) error {
	if err := d.Load(ctx); err != nil {
		return err
	}
	delay := d.connectBase
	for ctx.Err() == nil {
		client, err := d.client(ctx)
		if err != nil {
			d.recordStatus(ctx, store.StatusUnavailable, err)
			d.logger.Warn("Docker API not reachable", slog.String("error", err.Error()))
			select {
			case <-ctx.Done():
				return nil
			case <-d.clock.After(delay):
			}
			delay = min(delay*2, connectMax)
			continue
		}
		delay = d.connectBase
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
	return d.status == store.StatusOK
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
		d.recordStatus(ctx, store.StatusUnavailable, err)
		return fmt.Errorf("connect to the Docker API: %w", err)
	}
	listed, err := client.Containers(ctx)
	if err != nil {
		d.recordStatus(ctx, store.StatusUnavailable, err)
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
		d.recordStatus(ctx, store.StatusUnavailable, err)
		return err
	}
	d.publish(model)
	if failures > 0 {
		d.recordStatus(ctx, store.StatusDegraded,
			fmt.Errorf("%d of %d containers could not be inspected", failures, len(listed)))
		return nil
	}
	d.recordStatus(ctx, store.StatusOK, nil)
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
	if err := d.recordChanges(ctx, previous, model, now); err != nil {
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
// new container in a slot whose image differs from the previous container is
// a deploy, the same image is a recreation.
func (d *Discoverer) recordChanges(ctx context.Context, previous, model *Model, now time.Time) error {
	latest := latestSlotContainers(previous)
	for _, appName := range sortedKeys(model.Apps) {
		app := model.Apps[appName]
		for _, key := range sortedKeys(app.Slots) {
			slot := app.Slots[key]
			container := slot.Container
			if container == nil {
				continue
			}
			old := latest[app.Name+"\x00"+slot.Key]
			if old == nil || old.ID == container.ID {
				continue
			}
			if old.ImageID == container.ImageID {
				if err := d.recordRecreated(ctx, app, slot, container, now); err != nil {
					return err
				}
				continue
			}
			if err := d.recordDeploy(ctx, app, slot, old, container, now); err != nil {
				return err
			}
		}
	}
	return nil
}

// latestSlotContainers returns the newest container per app and slot key.
func latestSlotContainers(model *Model) map[string]*Container {
	latest := make(map[string]*Container)
	for _, container := range model.Containers {
		if container.App == nil || container.Slot == nil {
			continue
		}
		key := container.App.Name + "\x00" + container.Slot.Key
		if current, ok := latest[key]; !ok || current.CreatedAt.Before(container.CreatedAt) {
			latest[key] = container
		}
	}
	return latest
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
		recent, err := d.store.RecentEvent(ctx, app.ID, "deploy", at.Add(-deployMergeWindow))
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
		Kind:     "deploy",
		Severity: "info",
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
	return nil
}

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
		Kind:     "recreated",
		Severity: "info",
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
	delay := streamRetryBase
	for ctx.Err() == nil {
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
		select {
		case <-ctx.Done():
			return
		case <-d.clock.After(delay):
		}
		delay = min(delay*2, streamRetryMax)
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

// recordLifecycle stores one derived lifecycle event, attributed to the
// container's app and slot when they are known.
func (d *Discoverer) recordLifecycle(ctx context.Context, lifecycle LifecycleEvent) {
	event := store.Event{
		TS:       lifecycle.Time,
		Kind:     lifecycle.Kind,
		Severity: "info",
		Subject:  lifecycle.Name,
		Title:    lifecycleTitle(lifecycle),
	}
	payload := struct {
		Image    string `json:"image"`
		ExitCode *int   `json:"exit_code,omitempty"`
		OOM      *bool  `json:"oom,omitempty"`
		Health   string `json:"health,omitempty"`
	}{Image: lifecycle.ImageRef}
	switch lifecycle.Kind {
	case "crashed":
		exitCode := lifecycle.ExitCode
		oom := lifecycle.OOMKilled
		payload.ExitCode = &exitCode
		payload.OOM = &oom
		event.Severity = "warning"
	case "health":
		payload.Health = lifecycle.Health
	}
	data, err := json.Marshal(payload)
	if err != nil {
		d.logger.Warn("encode container event", slog.String("error", err.Error()))
		return
	}
	event.Data = data
	if container, ok := d.Model().Containers[lifecycle.ContainerID]; ok {
		event.Subject = container.Slot.Key
		if container.App.ID > 0 {
			appID := container.App.ID
			event.AppID = &appID
		}
		if container.Slot.ID > 0 {
			slotID := container.Slot.ID
			event.SlotID = &slotID
		}
	}
	if _, err := d.store.AddEvent(ctx, event); err != nil {
		d.logger.Warn("record container event", slog.String("kind", lifecycle.Kind), slog.String("error", err.Error()))
	}
}

// lifecycleTitle returns the one-line title of a lifecycle event.
func lifecycleTitle(lifecycle LifecycleEvent) string {
	switch lifecycle.Kind {
	case "crashed":
		if lifecycle.OOMKilled {
			return fmt.Sprintf("crashed: exit code %d, out of memory", lifecycle.ExitCode)
		}
		return fmt.Sprintf("crashed: exit code %d", lifecycle.ExitCode)
	case "health":
		return "health: " + lifecycle.Health
	default:
		return lifecycle.Kind
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

// recordStatus stores the state of the Docker source when it changed.
func (d *Discoverer) recordStatus(ctx context.Context, status string, cause error) {
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	d.mu.Lock()
	changed := d.status != status || d.statusError != message
	d.status = status
	d.statusError = message
	d.mu.Unlock()
	if !changed {
		return
	}
	if err := d.store.RecordSourceStatus(ctx, SourceName, status, message, d.clock.Now()); err != nil {
		d.logger.Warn("record Docker source status", slog.String("error", err.Error()))
	}
}

// publish makes the model current.
func (d *Discoverer) publish(model *Model) {
	d.mu.Lock()
	d.model = model
	d.mu.Unlock()
}

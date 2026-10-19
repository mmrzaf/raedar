//go:build integration

package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mmrzaf/raedar/internal/clock"
	"github.com/mmrzaf/raedar/internal/store"
	"github.com/mmrzaf/raedar/internal/testdb"
)

// testSchema creates a schema and returns a migrated store on it, the
// database URL and the schema name.
func testSchema(t *testing.T) (*store.Store, string, string) {
	t.Helper()
	ctx := context.Background()
	databaseURL, schema := testdb.NewSchema(t, "discovery")
	st, err := store.Open(ctx, testdb.WithSearchPath(t, databaseURL, schema), discardLogger())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	return st, databaseURL, schema
}

// testConn returns a connection to the test schema for direct queries.
func testConn(t *testing.T, databaseURL, schema string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), testdb.WithSearchPath(t, databaseURL, schema))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// portBinding is one published port of a fake container.
type portBinding struct {
	HostIP   string
	HostPort string
}

// testContainer is one container the fake daemon reports.
type testContainer struct {
	ID       string
	Name     string
	Image    string
	ImageID  string
	Project  string
	Service  string
	Replica  string
	Labels   map[string]string
	Created  time.Time
	State    string
	Ports    map[string][]portBinding
	Networks map[string]string
}

// labels returns the container labels, as the daemon reports them for a
// Compose container.
func (c testContainer) labels() map[string]string {
	labels := map[string]string{}
	if c.Project != "" {
		labels[labelComposeProject] = c.Project
	}
	if c.Service != "" {
		labels[labelComposeService] = c.Service
		number := c.Replica
		if number == "" {
			number = "1"
		}
		labels[labelComposeNumber] = number
	}
	for name, value := range c.Labels {
		labels[name] = value
	}
	return labels
}

// state returns the container state.
func (c testContainer) state() string {
	if c.State == "" {
		return "running"
	}
	return c.State
}

// listJSON returns the container list entry.
func (c testContainer) listJSON() map[string]any {
	return map[string]any{
		"Id":      c.ID,
		"Names":   []string{"/" + c.Name},
		"Image":   c.Image,
		"ImageID": c.ImageID,
		"Labels":  c.labels(),
		"State":   c.state(),
		"Created": c.Created.Unix(),
	}
}

// inspectJSON returns the inspect response.
func (c testContainer) inspectJSON() map[string]any {
	ports := map[string]any{}
	for key, bindings := range c.Ports {
		list := make([]map[string]string, 0, len(bindings))
		for _, binding := range bindings {
			list = append(list, map[string]string{"HostIp": binding.HostIP, "HostPort": binding.HostPort})
		}
		ports[key] = list
	}
	networks := map[string]any{}
	for name, address := range c.Networks {
		networks[name] = map[string]any{"IPAddress": address}
	}
	return map[string]any{
		"Id":      c.ID,
		"Name":    "/" + c.Name,
		"Image":   c.ImageID,
		"Created": c.Created.Format(time.RFC3339Nano),
		"State": map[string]any{
			"Status":    c.state(),
			"ExitCode":  0,
			"OOMKilled": false,
			"StartedAt": c.Created.Format(time.RFC3339Nano),
		},
		"Config":          map[string]any{"Image": c.Image, "Labels": c.labels(), "Tty": false},
		"HostConfig":      map[string]any{"RestartPolicy": map[string]any{"Name": "unless-stopped"}},
		"NetworkSettings": map[string]any{"Ports": ports, "Networks": networks},
	}
}

// fakeDocker is a Docker API stand-in the tests drive.
type fakeDocker struct {
	server *httptest.Server
	events chan []byte

	mu         sync.Mutex
	containers []testContainer
	fail       bool
}

// newFakeDocker starts the stand-in.
func newFakeDocker(t *testing.T) *fakeDocker {
	t.Helper()
	fake := &fakeDocker{events: make(chan []byte, 32)}
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Version":"27.1.1","ApiVersion":"1.46"}`))
	})
	mux.HandleFunc("/v1.46/containers/json", fake.handleList)
	mux.HandleFunc("/v1.46/containers/", fake.handleInspect)
	mux.HandleFunc("/v1.46/events", fake.handleEvents)
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

// set replaces the containers the daemon reports.
func (f *fakeDocker) set(containers ...testContainer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containers = containers
}

// setFail makes the daemon answer with errors.
func (f *fakeDocker) setFail(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = fail
}

// host returns the address to pass to the discoverer.
func (f *fakeDocker) host() string {
	return "tcp://" + strings.TrimPrefix(f.server.URL, "http://")
}

// pushEvent sends one container event to the event stream.
func (f *fakeDocker) pushEvent(action string, container testContainer, exitCode int) {
	attributes := container.labels()
	attributes["name"] = container.Name
	attributes["image"] = container.Image
	if exitCode >= 0 {
		attributes["exitCode"] = fmt.Sprint(exitCode)
	}
	event := map[string]any{
		"status": action,
		"id":     container.ID,
		"from":   container.Image,
		"Type":   "container",
		"Action": action,
		"time":   time.Now().Unix(),
		"Actor":  map[string]any{"ID": container.ID, "Attributes": attributes},
	}
	data, err := json.Marshal(event)
	if err != nil {
		return
	}
	f.events <- append(data, '\n')
}

// handleList answers the container list.
func (f *fakeDocker) handleList(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	fail := f.fail
	containers := append([]testContainer(nil), f.containers...)
	f.mu.Unlock()
	if fail {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"message":"cannot list containers"}`))
		return
	}
	listed := make([]map[string]any, 0, len(containers))
	for _, container := range containers {
		listed = append(listed, container.listJSON())
	}
	json.NewEncoder(w).Encode(listed)
}

// handleInspect answers one inspect request.
func (f *fakeDocker) handleInspect(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.46/containers/"), "/json")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"message":"cannot inspect containers"}`))
		return
	}
	for _, container := range f.containers {
		if container.ID == id {
			json.NewEncoder(w).Encode(container.inspectJSON())
			return
		}
	}
	w.WriteHeader(http.StatusNotFound)
	w.Write([]byte(`{"message":"No such container"}`))
}

// handleEvents streams container events.
func (f *fakeDocker) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming is not supported", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	for {
		select {
		case event := <-f.events:
			w.Write(event)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// newDiscoverer returns a discoverer over the fake daemon and store.
func newDiscoverer(t *testing.T, fake *fakeDocker, st *store.Store) *Discoverer {
	t.Helper()
	return New(fake.host(), "", "raedar-container", st, discardLogger(), clock.System())
}

// mustResync runs one resync that must succeed.
func mustResync(t *testing.T, d *Discoverer) {
	t.Helper()
	if err := d.Resync(context.Background()); err != nil {
		t.Fatalf("Resync() error = %v", err)
	}
}

// waitFor polls until check succeeds or the timeout passes.
func waitFor(t *testing.T, timeout time.Duration, check func() bool) {
	t.Helper()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(timeout)
	for {
		if check() {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatal("timed out waiting for the condition")
		}
	}
}

// countEvents returns how many events of one kind are stored.
func countEvents(t *testing.T, conn *pgx.Conn, kind string) int {
	t.Helper()
	var count int
	if err := conn.QueryRow(context.Background(), "select count(*) from events where kind = $1", kind).Scan(&count); err != nil {
		t.Fatalf("count %s events: %v", kind, err)
	}
	return count
}

// storedDeployData returns the payload of the one deploy event.
func storedDeployData(t *testing.T, conn *pgx.Conn) deployData {
	t.Helper()
	var data []byte
	if err := conn.QueryRow(context.Background(),
		"select data from events where kind = 'deploy' order by id limit 1").Scan(&data); err != nil {
		t.Fatalf("read deploy event: %v", err)
	}
	var payload deployData
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("decode deploy event: %v", err)
	}
	return payload
}

// apiContainer returns the shop api container with one image.
func apiContainer(id, image, imageID string, at time.Time) testContainer {
	return testContainer{
		ID:      id,
		Name:    "shop-api-1",
		Image:   image,
		ImageID: imageID,
		Project: "shop",
		Service: "api",
		Created: at,
		Labels:  map[string]string{"traefik.http.routers.shop-api.rule": "Host(`shop.example.com`)"},
	}
}

func TestResyncPersistsTheModel(t *testing.T) {
	fake := newFakeDocker(t)
	st, databaseURL, schema := testSchema(t)
	ctx := context.Background()
	base := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	fake.set(
		apiContainer("api1", "shop/api:1.0", "sha256:"+strings.Repeat("a", 64), base),
		testContainer{
			ID: "db1", Name: "shop-db-1", Image: "postgres:16", ImageID: "sha256:" + strings.Repeat("b", 64),
			Project: "shop", Service: "db", Created: base,
		},
	)
	d := newDiscoverer(t, fake, st)
	mustResync(t, d)
	model, err := st.LoadModel(ctx)
	if err != nil {
		t.Fatalf("LoadModel() error = %v", err)
	}
	if len(model.Apps) != 1 || model.Apps[0].Name != "shop" {
		t.Fatalf("apps = %+v, want shop", model.Apps)
	}
	if model.Apps[0].ArchivedAt != nil {
		t.Errorf("ArchivedAt = %v, want nil", model.Apps[0].ArchivedAt)
	}
	slotKeys := make([]string, 0, len(model.Slots))
	for _, slot := range model.Slots {
		slotKeys = append(slotKeys, slot.Key)
	}
	if want := []string{"api #1", "db #1"}; !reflect.DeepEqual(slotKeys, want) {
		t.Errorf("slot keys = %v, want %v", slotKeys, want)
	}
	if len(model.Containers) != 2 {
		t.Fatalf("containers = %+v, want 2", model.Containers)
	}
	if len(model.Routes) != 1 {
		t.Fatalf("routes = %+v, want 1", model.Routes)
	}
	route := model.Routes[0]
	if route.Name != "shop-api@docker" || route.Origin != "labels" || route.Kind != "http" {
		t.Errorf("route = %+v, want the label route", route)
	}
	if !reflect.DeepEqual(route.Domains, []string{"shop.example.com"}) {
		t.Errorf("domains = %v, want shop.example.com", route.Domains)
	}
	if route.AppID == nil || *route.AppID != model.Apps[0].ID {
		t.Errorf("route app = %v, want the shop app", route.AppID)
	}
	conn := testConn(t, databaseURL, schema)
	var status string
	if err := conn.QueryRow(ctx, "select status from source_state where source = 'docker'").Scan(&status); err != nil {
		t.Fatalf("read source state: %v", err)
	}
	if status != "ok" {
		t.Errorf("source status = %q, want ok", status)
	}
}

func TestResyncRecordsDeploysAndMerges(t *testing.T) {
	fake := newFakeDocker(t)
	st, databaseURL, schema := testSchema(t)
	base := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	conn := testConn(t, databaseURL, schema)
	d := newDiscoverer(t, fake, st)

	apiV1 := apiContainer("api1", "shop/api:1.0", "sha256:"+strings.Repeat("a", 64), base)
	fake.set(apiV1)
	mustResync(t, d)
	// The same image in a new container is a recreation, not a deploy.
	fake.set(apiContainer("api2", "shop/api:1.0", "sha256:"+strings.Repeat("a", 64), base.Add(time.Minute)))
	mustResync(t, d)
	if got := countEvents(t, conn, "recreated"); got != 1 {
		t.Errorf("recreated events = %d, want 1", got)
	}
	if got := countEvents(t, conn, "deploy"); got != 0 {
		t.Errorf("deploy events = %d, want 0", got)
	}
	// A different image is a deploy.
	apiV2 := apiContainer("api3", "shop/api:2.0", "sha256:"+strings.Repeat("c", 64), base.Add(2*time.Minute))
	fake.set(apiV2)
	mustResync(t, d)
	if got := countEvents(t, conn, "deploy"); got != 1 {
		t.Fatalf("deploy events = %d, want 1", got)
	}
	// A deploy of another slot of the same app within two minutes is merged
	// into the same event.
	web := testContainer{
		ID: "web1", Name: "shop-web-1", Image: "shop/web:1.0", ImageID: "sha256:" + strings.Repeat("d", 64),
		Project: "shop", Service: "web", Created: base,
	}
	fake.set(apiV2, web)
	mustResync(t, d)
	webV2 := web
	webV2.ID, webV2.Image = "web2", "shop/web:2.0"
	webV2.ImageID = "sha256:" + strings.Repeat("e", 64)
	webV2.Created = base.Add(3 * time.Minute)
	fake.set(apiV2, webV2)
	mustResync(t, d)
	if got := countEvents(t, conn, "deploy"); got != 1 {
		t.Fatalf("deploy events = %d, want 1 merged event", got)
	}
	merged := storedDeployData(t, conn)
	if want := []string{"api", "web"}; !reflect.DeepEqual(merged.Services, want) {
		t.Errorf("services = %v, want %v", merged.Services, want)
	}
	if len(merged.Changes) != 2 {
		t.Fatalf("changes = %+v, want 2", merged.Changes)
	}
	first := merged.Changes[0]
	if first.Slot != "api #1" || first.OldImage != "shop/api:1.0" || first.NewImage != "shop/api:2.0" {
		t.Errorf("first change = %+v, want the api upgrade", first)
	}
	if first.OldImageID != strings.Repeat("a", 12) || first.NewImageID != strings.Repeat("c", 12) {
		t.Errorf("image IDs = %q, %q, want the short IDs", first.OldImageID, first.NewImageID)
	}
	// A deploy in another app is a separate event.
	gitman := testContainer{
		ID: "git1", Name: "gitman-1", Image: "gitman:1.0", ImageID: "sha256:" + strings.Repeat("f", 64),
		Project: "gitman", Service: "gitman", Created: base,
	}
	fake.set(apiV2, webV2, gitman)
	mustResync(t, d)
	gitmanV2 := gitman
	gitmanV2.ID, gitmanV2.Image = "git2", "gitman:1.1"
	gitmanV2.ImageID = "sha256:" + strings.Repeat("0", 64)
	gitmanV2.Created = base.Add(4 * time.Minute)
	fake.set(apiV2, webV2, gitmanV2)
	mustResync(t, d)
	if got := countEvents(t, conn, "deploy"); got != 2 {
		t.Errorf("deploy events = %d, want 2", got)
	}
}

func TestResyncFindsDeploysAfterRestart(t *testing.T) {
	fake := newFakeDocker(t)
	st, databaseURL, schema := testSchema(t)
	ctx := context.Background()
	base := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	fake.set(apiContainer("api1", "shop/api:1.0", "sha256:"+strings.Repeat("a", 64), base))
	mustResync(t, newDiscoverer(t, fake, st))

	fake.set(apiContainer("api2", "shop/api:2.0", "sha256:"+strings.Repeat("c", 64), base.Add(time.Hour)))
	second := newDiscoverer(t, fake, st)
	if err := second.Load(ctx); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	mustResync(t, second)
	data := storedDeployData(t, testConn(t, databaseURL, schema))
	if len(data.Changes) != 1 || data.Changes[0].OldImage != "shop/api:1.0" || data.Changes[0].NewImage != "shop/api:2.0" {
		t.Errorf("changes = %+v, want the deploy across the restart", data.Changes)
	}
}

func TestResyncFindsDeploysWhenTheOldContainerIsRemovedFirst(t *testing.T) {
	fake := newFakeDocker(t)
	st, databaseURL, schema := testSchema(t)
	base := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	d := newDiscoverer(t, fake, st)
	fake.set(apiContainer("api1", "shop/api:1.0", "sha256:"+strings.Repeat("a", 64), base))
	mustResync(t, d)
	// docker rm -f followed by docker run: a resync sees no container in
	// between, so the previous model has forgotten the old one.
	fake.set()
	mustResync(t, d)
	fake.set(apiContainer("api2", "shop/api:2.0", "sha256:"+strings.Repeat("c", 64), base.Add(time.Hour)))
	mustResync(t, d)
	data := storedDeployData(t, testConn(t, databaseURL, schema))
	if len(data.Changes) != 1 || data.Changes[0].OldImage != "shop/api:1.0" || data.Changes[0].NewImage != "shop/api:2.0" {
		t.Errorf("changes = %+v, want the deploy across the gap", data.Changes)
	}
}

func TestResyncIgnoresOneOffAndIgnoredContainers(t *testing.T) {
	fake := newFakeDocker(t)
	st, databaseURL, schema := testSchema(t)
	base := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	d := newDiscoverer(t, fake, st)
	api := apiContainer("api1", "shop/api:1.0", "sha256:"+strings.Repeat("a", 64), base)
	oneOff := apiContainer("run1", "shop/api:1.0", "sha256:"+strings.Repeat("a", 64), base.Add(time.Minute))
	oneOff.Name = "shop-api-run-1a2b"
	oneOff.Labels = map[string]string{labelComposeOneOff: "True"}
	ignored := testContainer{
		ID: "tool1", Name: "shop-tool-1", Image: "shop/tool:1.0", ImageID: "sha256:" + strings.Repeat("b", 64),
		Project: "shop", Service: "tool", Created: base, Labels: map[string]string{labelIgnore: "true"},
	}
	fake.set(api, oneOff, ignored)
	mustResync(t, d)
	conn := testConn(t, databaseURL, schema)
	var containers, slots int
	if err := conn.QueryRow(context.Background(), "select (select count(*) from containers), (select count(*) from slots)").Scan(&containers, &slots); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if containers != 1 || slots != 1 {
		t.Errorf("containers = %d, slots = %d, want only the service container", containers, slots)
	}
	if got := countEvents(t, conn, "recreated"); got != 0 {
		t.Errorf("recreated events = %d, want 0: a one-off run is not a recreation", got)
	}
	if running := d.RunningContainers(); len(running) != 1 || running[0].ID != "api1" {
		t.Errorf("RunningContainers() = %+v, want only the service container", running)
	}
}

func TestResyncKeepsAccessLogRoutes(t *testing.T) {
	fake := newFakeDocker(t)
	st, databaseURL, schema := testSchema(t)
	ctx := context.Background()
	conn := testConn(t, databaseURL, schema)
	var appID int64
	if err := conn.QueryRow(ctx,
		`insert into apps (name, first_seen, last_seen) values ('edge', now(), now()) returning id`).Scan(&appID); err != nil {
		t.Fatalf("insert app: %v", err)
	}
	if _, err := conn.Exec(ctx,
		`insert into routes (name, app_id, kind, origin, first_seen, last_seen)
		 values ('git@file', $1, 'http', 'accesslog', now(), now())`, appID); err != nil {
		t.Fatalf("insert route: %v", err)
	}
	fake.set(apiContainer("api1", "shop/api:1.0", "sha256:"+strings.Repeat("a", 64), time.Now().UTC()))
	d := newDiscoverer(t, fake, st)
	if err := d.Load(ctx); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if _, ok := d.Model().Routes["git@file"]; !ok {
		t.Fatalf("loaded routes = %v, want the access log route", d.Model().Routes)
	}
	mustResync(t, d)
	route := d.Model().Routes["git@file"]
	if route == nil || route.Origin != "accesslog" {
		t.Errorf("route after resync = %+v, want the access log route", route)
	}
}

func TestResyncRecordsFailureStatus(t *testing.T) {
	fake := newFakeDocker(t)
	st, databaseURL, schema := testSchema(t)
	ctx := context.Background()
	fake.set(apiContainer("api1", "shop/api:1.0", "sha256:"+strings.Repeat("a", 64), time.Now().UTC()))
	d := newDiscoverer(t, fake, st)
	mustResync(t, d)
	fake.setFail(true)
	if err := d.Resync(ctx); err == nil {
		t.Fatal("Resync() error = nil, want an error")
	}
	conn := testConn(t, databaseURL, schema)
	var status string
	var lastError *string
	if err := conn.QueryRow(ctx,
		"select status, last_error from source_state where source = 'docker'").Scan(&status, &lastError); err != nil {
		t.Fatalf("read source state: %v", err)
	}
	if status != "unavailable" {
		t.Errorf("source status = %q, want unavailable", status)
	}
	if lastError == nil || !strings.Contains(*lastError, "list containers") {
		t.Errorf("last error = %v, want the list failure", lastError)
	}
}

func TestResyncMarksRemovedContainers(t *testing.T) {
	fake := newFakeDocker(t)
	st, databaseURL, schema := testSchema(t)
	base := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	api := apiContainer("api1", "shop/api:1.0", "sha256:"+strings.Repeat("a", 64), base)
	db := testContainer{
		ID: "db1", Name: "shop-db-1", Image: "postgres:16", ImageID: "sha256:" + strings.Repeat("b", 64),
		Project: "shop", Service: "db", Created: base,
	}
	fake.set(api, db)
	d := newDiscoverer(t, fake, st)
	mustResync(t, d)
	fake.set(api)
	mustResync(t, d)
	conn := testConn(t, databaseURL, schema)
	var removedAt *time.Time
	if err := conn.QueryRow(context.Background(),
		"select removed_at from containers where docker_id = 'db1'").Scan(&removedAt); err != nil {
		t.Fatalf("read container: %v", err)
	}
	if removedAt == nil {
		t.Error("removed_at = nil, want the removal to be recorded")
	}
}

func TestDetectsTraefikAndTLSAddress(t *testing.T) {
	fake := newFakeDocker(t)
	st, _, _ := testSchema(t)
	base := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	fake.set(
		testContainer{
			ID: "traefik1", Name: "traefik", Image: "traefik:v3.1", ImageID: "sha256:" + strings.Repeat("a", 64),
			Created: base, Ports: map[string][]portBinding{"443/tcp": {{HostPort: "443"}}},
			Networks: map[string]string{"proxy": "172.20.0.2", "socket": "172.21.0.2"},
		},
		testContainer{
			ID: "raedar-container-0001", Name: "raedar", Image: "raedar:1.0", ImageID: "sha256:" + strings.Repeat("b", 64),
			Created: base, Networks: map[string]string{"proxy": "172.20.0.9"},
		},
	)
	d := New(fake.host(), "", "raedar-container", st, discardLogger(), clock.System())
	mustResync(t, d)
	model := d.Model()
	if model.Traefik == nil || model.Traefik.Container == nil {
		t.Fatalf("Traefik = %+v, want the detected container", model.Traefik)
	}
	if got := model.Traefik.TLSCheckAddr; got != "172.20.0.2:443" {
		t.Errorf("TLSCheckAddr = %q, want 172.20.0.2:443", got)
	}
}

func TestRunRecordsLifecycleOnlyForTrackedContainers(t *testing.T) {
	fake := newFakeDocker(t)
	st, databaseURL, schema := testSchema(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	api := apiContainer("api1", "shop/api:1.0", "sha256:"+strings.Repeat("a", 64), base)
	fake.set(api)
	d := newDiscoverer(t, fake, st)
	d.resyncEvery = time.Hour
	done := make(chan error, 1)
	if err := d.Load(ctx); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	go func() { done <- d.Run(ctx) }()
	conn := testConn(t, databaseURL, schema)
	waitFor(t, 10*time.Second, func() bool {
		var count int
		return conn.QueryRow(ctx, "select count(*) from containers").Scan(&count) == nil && count == 1
	})
	// A short-lived container of some tool, a one-off run and the service
	// itself, which was recreated and has not been resynced yet.
	stray := testContainer{ID: "tmp1", Name: "gitman-9f2c-3", Image: "gitman:1.0"}
	oneOff := apiContainer("run1", "shop/api:1.0", "", base)
	oneOff.Name = "shop-api-run-1a2b"
	oneOff.Labels = map[string]string{labelComposeOneOff: "True"}
	fresh := apiContainer("api2", "shop/api:1.0", "", base)
	for _, container := range []testContainer{stray, oneOff, fresh} {
		fake.pushEvent("start", container, -1)
	}
	// A restart is a start followed by a restart event and must count once.
	fake.pushEvent("restart", fresh, -1)
	waitFor(t, 15*time.Second, func() bool { return countEvents(t, conn, "started") >= 1 })
	time.Sleep(500 * time.Millisecond)
	rows, err := conn.Query(ctx, "select subject, app_id is not null, slot_id is not null from events where kind = 'started'")
	if err != nil {
		t.Fatalf("read started events: %v", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var (
			subject         string
			hasApp, hasSlot bool
		)
		if err := rows.Scan(&subject, &hasApp, &hasSlot); err != nil {
			t.Fatalf("scan event: %v", err)
		}
		count++
		if subject != "api #1" || !hasApp || !hasSlot {
			t.Errorf("event = %q app %v slot %v, want the api slot with its app", subject, hasApp, hasSlot)
		}
	}
	if count != 1 {
		t.Errorf("started events = %d, want 1: only the tracked container, once", count)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run() did not stop")
	}
}

func TestRunClassifiesCrashesFromTheEventStream(t *testing.T) {
	fake := newFakeDocker(t)
	st, databaseURL, schema := testSchema(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	api := apiContainer("api1", "shop/api:1.0", "sha256:"+strings.Repeat("a", 64), base)
	fake.set(api)
	d := newDiscoverer(t, fake, st)
	d.resyncEvery = 100 * time.Millisecond
	done := make(chan error, 1)
	if err := d.Load(ctx); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	go func() { done <- d.Run(ctx) }()
	conn := testConn(t, databaseURL, schema)
	waitFor(t, 10*time.Second, func() bool {
		var count int
		return conn.QueryRow(ctx, "select count(*) from containers").Scan(&count) == nil && count == 1
	})
	fake.pushEvent("die", api, 1)
	waitFor(t, 15*time.Second, func() bool {
		return countEvents(t, conn, "crashed") == 1
	})
	var subject, title string
	var data []byte
	if err := conn.QueryRow(ctx,
		"select subject, title, data from events where kind = 'crashed'").Scan(&subject, &title, &data); err != nil {
		t.Fatalf("read crashed event: %v", err)
	}
	if subject != "api #1" {
		t.Errorf("subject = %q, want api #1", subject)
	}
	if title != "crashed: exit code 1" {
		t.Errorf("title = %q, want the exit code", title)
	}
	var payload struct {
		ExitCode *int  `json:"exit_code"`
		OOM      *bool `json:"oom"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("decode crash payload: %v", err)
	}
	if payload.ExitCode == nil || *payload.ExitCode != 1 || payload.OOM == nil || *payload.OOM {
		t.Errorf("payload = %+v, want exit code 1 without oom", payload)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run() error = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run() did not stop")
	}
}

func TestLoadRestoresTheRoutesOfEachApp(t *testing.T) {
	fake := newFakeDocker(t)
	st, _, _ := testSchema(t)
	base := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	fake.set(apiContainer("api1", "shop/api:1.0", "sha256:"+strings.Repeat("a", 64), base))
	mustResync(t, newDiscoverer(t, fake, st))

	// A restarted process knows the app's routes before Docker answers, so
	// its charts work while the daemon is unreachable.
	restarted := newDiscoverer(t, fake, st)
	if err := restarted.Load(context.Background()); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	app := restarted.Model().Apps["shop"]
	if app == nil || app.Routes["shop-api@docker"] == nil {
		t.Fatalf("app = %+v, want it to carry its route after Load", app)
	}
	if route := restarted.Model().Routes["shop-api@docker"]; route == nil || route.App != app || route.ID == 0 {
		t.Errorf("route = %+v, want it attached to its app with its row ID", route)
	}
}

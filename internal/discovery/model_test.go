package discovery

import (
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/mmrzaf/raedar/internal/config"
	"github.com/mmrzaf/raedar/internal/docker"
)

// discardLogger returns a logger that writes nothing.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// inspectFixture is a minimal inspect response for model tests.
func inspectFixture(id, name string, labels map[string]string) docker.ContainerInspect {
	return docker.ContainerInspect{
		ID:        id,
		Name:      name,
		ImageRef:  "shop/api:1.0",
		ImageID:   "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		Labels:    labels,
		State:     "running",
		Created:   time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC),
		StartedAt: time.Date(2026, 10, 18, 10, 0, 1, 0, time.UTC),
	}
}

func TestIsCatchAllRoute(t *testing.T) {
	tests := map[string]bool{
		UnroutedRoute:     true,
		"fallback@file":   true,
		"fallback@docker": true,
		"shop@docker":     false,
		"myfallback@file": false,
	}
	for name, want := range tests {
		if got := IsCatchAllRoute(name); got != want {
			t.Errorf("IsCatchAllRoute(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestAppNamePrecedence(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{
			name:   "raedar.app wins",
			labels: map[string]string{labelApp: "custom", labelComposeProject: "shop"},
			want:   "custom",
		},
		{
			name:   "compose project",
			labels: map[string]string{labelComposeProject: "shop"},
			want:   "shop",
		},
		{
			name: "container name",
			want: "standalone-web",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inspect := inspectFixture("c1", "standalone-web", tt.labels)
			if got := identify(inspect.Name, inspect.Labels).App; got != tt.want {
				t.Errorf("identify().App = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSlotKey(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{
			name: "compose service and number",
			labels: map[string]string{
				labelComposeService: "api",
				labelComposeNumber:  "2",
			},
			want: "api #2",
		},
		{
			name:   "compose service without number falls back to the name",
			labels: map[string]string{labelComposeService: "api"},
			want:   "standalone-web",
		},
		{
			name: "standalone container",
			want: "standalone-web",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inspect := inspectFixture("c1", "standalone-web", tt.labels)
			if got := identify(inspect.Name, inspect.Labels).SlotKey; got != tt.want {
				t.Errorf("identify().SlotKey = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildGroupsContainers(t *testing.T) {
	newer := inspectFixture("c2", "shop-api-1", map[string]string{
		labelComposeProject:                  "shop",
		labelComposeService:                  "api",
		labelComposeNumber:                   "1",
		"traefik.http.routers.shop-api.rule": "Host(`shop.example.com`)",
	})
	newer.Created = newer.Created.Add(time.Hour)
	older := inspectFixture("c1", "shop-api-old", map[string]string{
		labelComposeProject: "shop",
		labelComposeService: "api",
		labelComposeNumber:  "1",
	})
	database := inspectFixture("c3", "shop-db-1", map[string]string{
		labelComposeProject: "shop",
		labelComposeService: "db",
		labelComposeNumber:  "1",
	})

	model := build([]docker.ContainerInspect{newer, older, database}, "", "", discardLogger())

	if len(model.Apps) != 1 {
		t.Fatalf("apps = %d, want 1", len(model.Apps))
	}
	app := model.Apps["shop"]
	if app == nil {
		t.Fatal("app shop not found")
	}
	if len(app.Slots) != 2 {
		t.Fatalf("slots = %d, want 2", len(app.Slots))
	}
	api := app.Slots["api #1"]
	if api == nil {
		t.Fatal("slot api #1 not found")
	}
	if api.Service != "api" || api.Replica != 1 {
		t.Errorf("slot = %+v, want service api replica 1", api)
	}
	if api.Container == nil || api.Container.ID != "c2" {
		t.Errorf("slot container = %+v, want the newest container c2", api.Container)
	}
	if len(app.Routes) != 1 || app.Routes["shop-api@docker"] == nil {
		t.Fatalf("routes = %+v, want shop-api@docker", app.Routes)
	}
	if got := app.Routes["shop-api@docker"].Domains; !reflect.DeepEqual(got, []string{"shop.example.com"}) {
		t.Errorf("route domains = %v, want shop.example.com", got)
	}
	if model.Containers["c1"].Slot != api {
		t.Errorf("older container slot = %+v, want api #1", model.Containers["c1"].Slot)
	}
}

func TestBuildRejectsDisabledRouters(t *testing.T) {
	container := inspectFixture("c1", "shop-api-1", map[string]string{
		labelComposeProject:                  "shop",
		labelComposeService:                  "api",
		labelComposeNumber:                   "1",
		"traefik.enable":                     "false",
		"traefik.http.routers.shop-api.rule": "Host(`shop.example.com`)",
	})
	model := build([]docker.ContainerInspect{container}, "", "", discardLogger())
	if len(model.Routes) != 0 {
		t.Errorf("routes = %+v, want none", model.Routes)
	}
}

func TestBuildResolvesAppLabels(t *testing.T) {
	oldIgnored := inspectFixture("c1", "shop-api-1", map[string]string{
		labelComposeProject: "shop",
		labelComposeService: "api",
		labelComposeNumber:  "1",
		labelIgnore:         "true",
		labelAlerts:         "true",
	})
	newerCritical := inspectFixture("c2", "shop-api-2", map[string]string{
		labelComposeProject: "shop",
		labelComposeService: "api",
		labelComposeNumber:  "2",
		labelCritical:       "true",
		labelLatencyIgnore:  "/ws, /events",
		labelErrorsMinRate:  "5%",
		labelSlowMinP95:     "800ms",
	})
	newerCritical.Created = newerCritical.Created.Add(time.Hour)
	model := build([]docker.ContainerInspect{oldIgnored, newerCritical}, "", "", discardLogger())
	app := model.Apps["shop"]
	if app == nil {
		t.Fatal("app shop not found")
	}
	if !app.Critical {
		t.Error("Critical = false, want true")
	}
	if !app.Alerts {
		t.Error("Alerts = false, want the default true")
	}
	if len(app.Slots) != 1 {
		t.Errorf("slots = %v, want only the container that is not ignored", app.Slots)
	}
	if want := []string{"/ws", "/events"}; !reflect.DeepEqual(app.LatencyIgnore, want) {
		t.Errorf("LatencyIgnore = %v, want %v", app.LatencyIgnore, want)
	}
	if app.ErrorsMinRate == nil || *app.ErrorsMinRate != config.Percent(5) {
		t.Errorf("ErrorsMinRate = %v, want 5%%", app.ErrorsMinRate)
	}
	if app.SlowMinP95 == nil || *app.SlowMinP95 != 800*time.Millisecond {
		t.Errorf("SlowMinP95 = %v, want 800ms", app.SlowMinP95)
	}
}

func TestBuildSkipsIgnoredApps(t *testing.T) {
	first := inspectFixture("c1", "shop-api-1", map[string]string{
		labelComposeProject: "shop",
		labelComposeService: "api",
		labelComposeNumber:  "1",
		labelIgnore:         "true",
	})
	second := inspectFixture("c2", "shop-db-1", map[string]string{
		labelComposeProject: "shop",
		labelComposeService: "db",
		labelComposeNumber:  "1",
		labelIgnore:         "TRUE",
	})
	second.Labels["traefik.http.routers.shop-db.rule"] = "Host(`db.example.com`)"
	model := build([]docker.ContainerInspect{first, second}, "", "", discardLogger())
	if app := model.Apps["shop"]; app != nil {
		t.Errorf("app = %+v, want no app when every container is ignored", app)
	}
	if len(model.Routes) != 0 {
		t.Errorf("routes = %+v, want none for ignored containers", model.Routes)
	}
	for _, id := range []string{"c1", "c2"} {
		container := model.Containers[id]
		if container == nil || container.Slot != nil || container.App != nil {
			t.Errorf("container %s = %+v, want it known but without app or slot", id, container)
		}
	}
}

func TestBuildSkipsOneOffContainers(t *testing.T) {
	service := inspectFixture("c1", "shop-api-1", map[string]string{
		labelComposeProject: "shop",
		labelComposeService: "api",
		labelComposeNumber:  "1",
		labelComposeOneOff:  "False",
	})
	oneOff := inspectFixture("c2", "shop-api-run-1a2b3c", map[string]string{
		labelComposeProject: "shop",
		labelComposeService: "api",
		labelComposeNumber:  "1",
		labelComposeOneOff:  "True",
	})
	oneOff.Created = oneOff.Created.Add(time.Hour)
	model := build([]docker.ContainerInspect{service, oneOff}, "", "", discardLogger())
	slot := model.Apps["shop"].Slots["api #1"]
	if slot == nil || slot.Container == nil || slot.Container.ID != "c1" {
		t.Fatalf("slot = %+v, want the service container, not the newer one-off", slot)
	}
	if got := model.Containers["c2"]; got == nil || got.Slot != nil {
		t.Errorf("one-off container = %+v, want it known but without a slot", got)
	}
}

func TestIdentify(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   identity
	}{
		{
			name: "compose replica",
			labels: map[string]string{
				labelComposeProject: "shop", labelComposeService: "api", labelComposeNumber: "2",
			},
			want: identity{App: "shop", SlotKey: "api #2", Service: "api", Replica: 2},
		},
		{
			name:   "standalone",
			labels: nil,
			want:   identity{App: "c", SlotKey: "c", Service: "c", Replica: 1},
		},
		{
			name:   "ignored and one-off flags",
			labels: map[string]string{labelIgnore: "true", labelComposeOneOff: "True"},
			want:   identity{App: "c", SlotKey: "c", Service: "c", Replica: 1, Ignored: true, OneOff: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := identify("c", tt.labels); got != tt.want {
				t.Errorf("identify() = %+v, want %+v", got, tt.want)
			}
		})
	}
	if !identify("c", nil).Tracked() || identify("c", map[string]string{labelIgnore: "true"}).Tracked() {
		t.Error("Tracked() must be true for plain containers and false for ignored ones")
	}
}

func TestContainerFinished(t *testing.T) {
	tests := []struct {
		name      string
		container Container
		want      bool
	}{
		{name: "migration job", container: Container{State: "exited", ExitCode: 0, RestartPolicy: "no"}, want: true},
		{name: "crashed", container: Container{State: "exited", ExitCode: 1, RestartPolicy: "no"}},
		{name: "restarts on exit", container: Container{State: "exited", ExitCode: 0, RestartPolicy: "always"}},
		{name: "running", container: Container{State: "running", RestartPolicy: "no"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.container.Finished(); got != tt.want {
				t.Errorf("Finished() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildInvalidLabelValues(t *testing.T) {
	container := inspectFixture("c1", "shop-api-1", map[string]string{
		labelComposeProject: "shop",
		labelComposeService: "api",
		labelComposeNumber:  "1",
		labelErrorsMinRate:  "often",
		labelSlowMinP95:     "a while",
	})
	model := build([]docker.ContainerInspect{container}, "", "", discardLogger())
	app := model.Apps["shop"]
	if app == nil {
		t.Fatal("app shop not found")
	}
	if app.ErrorsMinRate != nil {
		t.Errorf("ErrorsMinRate = %v, want nil for an invalid label", app.ErrorsMinRate)
	}
	if app.SlowMinP95 != nil {
		t.Errorf("SlowMinP95 = %v, want nil for an invalid label", app.SlowMinP95)
	}
}

func TestDetectTraefik(t *testing.T) {
	withPort := func(container *Container) *Container {
		container.Ports = map[string][]docker.PortBinding{"443/tcp": {{HostPort: "443"}}}
		return container
	}
	plain := &Container{ID: "t1", Name: "traefik", ImageRef: "traefik:v3.1", State: "running"}
	labelled := &Container{ID: "t2", Name: "edge", ImageRef: "ghcr.io/other:1", State: "running",
		Labels: map[string]string{labelTraefik: "true"}}
	second := withPort(&Container{ID: "t3", Name: "traefik-2", ImageRef: "docker.io/traefik:v3.1", State: "running"})

	tests := []struct {
		name       string
		configured string
		containers []*Container
		wantID     string
	}{
		{name: "configured name", configured: "edge", containers: []*Container{plain, labelled}, wantID: "t2"},
		{name: "label", containers: []*Container{plain, labelled}, wantID: "t2"},
		{name: "image repository", containers: []*Container{plain}, wantID: "t1"},
		{name: "image with registry", containers: []*Container{second}, wantID: "t3"},
		{name: "several images, the one publishing 443", containers: []*Container{plain, second}, wantID: "t3"},
		{name: "not running", containers: []*Container{{ID: "t4", Name: "traefik", ImageRef: "traefik", State: "exited"}}},
		{name: "not traefik", containers: []*Container{{ID: "t5", Name: "web", ImageRef: "nginx:1", State: "running"}}},
		{name: "none", containers: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			traefik := detectTraefik(tt.configured, tt.containers, "")
			if tt.wantID == "" {
				if traefik != nil {
					t.Fatalf("detectTraefik() = %+v, want nil", traefik.Container)
				}
				return
			}
			if traefik == nil || traefik.Container == nil || traefik.Container.ID != tt.wantID {
				t.Fatalf("detectTraefik() = %+v, want container %s", traefik, tt.wantID)
			}
		})
	}
}

func TestTLSCheckAddr(t *testing.T) {
	traefik := &Container{
		Name:  "traefik",
		Ports: map[string][]docker.PortBinding{"8443/tcp": {{HostPort: "443"}}},
		Networks: []docker.Network{
			{Name: "atlas_web", IPAddress: "172.20.0.2"},
			{Name: "socket", IPAddress: "172.21.0.2"},
		},
	}
	tests := []struct {
		name string
		self *Container
		want string
	}{
		{
			name: "shared network",
			self: &Container{Networks: []docker.Network{{Name: "atlas_web", IPAddress: "172.20.0.9"}}},
			want: "172.20.0.2:8443",
		},
		{
			name: "no shared network",
			self: &Container{Networks: []docker.Network{{Name: "data", IPAddress: "172.22.0.9"}}},
		},
		{name: "own container unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tlsCheckAddr(traefik, tt.self); got != tt.want {
				t.Errorf("tlsCheckAddr() = %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("no published port", func(t *testing.T) {
		if got := tlsCheckAddr(&Container{Ports: map[string][]docker.PortBinding{}}, &Container{}); got != "" {
			t.Errorf("tlsCheckAddr() = %q, want empty", got)
		}
	})
}

func TestSelfContainer(t *testing.T) {
	containers := []*Container{
		{ID: "aaaaaaaaaaaa11111111111111111111111111111111111111111111111111111111", Name: "shop-api-1"},
		{ID: "bbbbbbbbbbbb22222222222222222222222222222222222222222222222222222222", Name: "raedar"},
	}
	tests := []struct {
		name     string
		hostname string
		want     string
	}{
		{name: "matches the ID prefix", hostname: "bbbbbbbbbbbb", want: "raedar"},
		{name: "no match", hostname: "cccccccccccc"},
		{name: "empty hostname"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := selfContainer(containers, tt.hostname)
			if tt.want == "" {
				if got != nil {
					t.Fatalf("selfContainer() = %+v, want nil", got)
				}
				return
			}
			if got == nil || got.Name != tt.want {
				t.Fatalf("selfContainer() = %+v, want %q", got, tt.want)
			}
		})
	}
}

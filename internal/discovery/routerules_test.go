package discovery

import (
	"reflect"
	"testing"
)

func TestRuleDomains(t *testing.T) {
	tests := []struct {
		name string
		rule string
		want []string
	}{
		{
			name: "v3 form",
			rule: "Host(`a.example.com`) || Host(`b.example.com`)",
			want: []string{"a.example.com", "b.example.com"},
		},
		{
			name: "v2 form",
			rule: "Host(`a.example.com`, `b.example.com`)",
			want: []string{"a.example.com", "b.example.com"},
		},
		{
			name: "with another matcher",
			rule: "Host(`shop.example.com`) && PathPrefix(`/api`)",
			want: []string{"shop.example.com"},
		},
		{
			name: "HostSNI",
			rule: "HostSNI(`api.example.com`)",
			want: []string{"api.example.com"},
		},
		{
			name: "wildcard HostSNI",
			rule: "HostSNI(`*`)",
		},
		{
			name: "HostRegexp contributes nothing",
			rule: "HostRegexp(`{subdomain:[a-z]+}.example.com`)",
		},
		{
			name: "HostSNIRegexp contributes nothing",
			rule: "HostSNIRegexp(`{subdomain:[a-z]+}.example.com`)",
		},
		{
			name: "regexp next to host",
			rule: "Host(`a.example.com`) || HostRegexp(`{x}.example.com`) && HostSNI(`b.example.com`)",
			want: []string{"a.example.com", "b.example.com"},
		},
		{
			name: "wildcard host",
			rule: "Host(`*.example.com`)",
			want: []string{"*.example.com"},
		},
		{
			name: "double quotes",
			rule: `Host("a.example.com")`,
			want: []string{"a.example.com"},
		},
		{
			name: "duplicate host",
			rule: "Host(`a.example.com`) || Host(`a.example.com`)",
			want: []string{"a.example.com"},
		},
		{
			name: "matcher name inside another word",
			rule: "PathPrefix(`/MyHost(`)",
		},
		{
			name: "no matcher",
			rule: "Method(`GET`)",
		},
		{
			name: "empty rule",
			rule: "",
		},
		{
			name: "unclosed matcher",
			rule: "Host(`a.example.com`",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ruleDomains(tt.rule); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ruleDomains(%q) = %v, want %v", tt.rule, got, tt.want)
			}
		})
	}
}

func TestRoutesFromLabels(t *testing.T) {
	t.Run("http router with TLS", func(t *testing.T) {
		routes := routesFromLabels(map[string]string{
			"traefik.enable":                                 "true",
			"traefik.http.routers.shop-api.rule":             "Host(`shop.example.com`)",
			"traefik.http.routers.shop-api.entrypoints":      "websecure",
			"traefik.http.routers.shop-api.service":          "shop-api",
			"traefik.http.routers.shop-api.tls":              "true",
			"traefik.http.routers.shop-api.tls.certresolver": "letsencrypt",
		})
		want := []Route{{
			Name:        "shop-api@docker",
			Kind:        "http",
			Rule:        "Host(`shop.example.com`)",
			Domains:     []string{"shop.example.com"},
			TLS:         true,
			Origin:      "labels",
			Entrypoints: []string{"websecure"},
			Service:     "shop-api",
		}}
		if !reflect.DeepEqual(routes, want) {
			t.Errorf("routesFromLabels() = %+v, want %+v", routes, want)
		}
	})

	t.Run("certresolver implies TLS", func(t *testing.T) {
		routes := routesFromLabels(map[string]string{
			"traefik.http.routers.api.rule":             "Host(`api.example.com`)",
			"traefik.http.routers.api.tls.certresolver": "letsencrypt",
		})
		if len(routes) != 1 || !routes[0].TLS {
			t.Fatalf("routesFromLabels() = %+v, want one TLS route", routes)
		}
	})

	t.Run("explicit tls false", func(t *testing.T) {
		routes := routesFromLabels(map[string]string{
			"traefik.http.routers.api.rule": "Host(`api.example.com`)",
			"traefik.http.routers.api.tls":  "false",
		})
		if len(routes) != 1 || routes[0].TLS {
			t.Fatalf("routesFromLabels() = %+v, want one route without TLS", routes)
		}
	})

	t.Run("tcp router with passthrough", func(t *testing.T) {
		routes := routesFromLabels(map[string]string{
			"traefik.tcp.routers.git.rule":            "HostSNI(`git.example.com`)",
			"traefik.tcp.routers.git.tls.passthrough": "true",
		})
		want := []Route{{
			Name:        "git@docker",
			Kind:        "tcp",
			Rule:        "HostSNI(`git.example.com`)",
			Domains:     []string{"git.example.com"},
			Passthrough: true,
			Origin:      "labels",
		}}
		if !reflect.DeepEqual(routes, want) {
			t.Errorf("routesFromLabels() = %+v, want %+v", routes, want)
		}
	})

	t.Run("disabled router", func(t *testing.T) {
		routes := routesFromLabels(map[string]string{
			"traefik.enable":                "false",
			"traefik.http.routers.api.rule": "Host(`api.example.com`)",
		})
		if routes != nil {
			t.Errorf("routesFromLabels() = %+v, want none", routes)
		}
	})

	t.Run("router without rule is dropped", func(t *testing.T) {
		routes := routesFromLabels(map[string]string{
			"traefik.http.routers.api.entrypoints": "web",
		})
		if routes != nil {
			t.Errorf("routesFromLabels() = %+v, want none", routes)
		}
	})

	t.Run("unknown fields are ignored", func(t *testing.T) {
		routes := routesFromLabels(map[string]string{
			"traefik.http.routers.api.rule":                "Host(`api.example.com`)",
			"traefik.http.routers.api.priority":            "10",
			"traefik.http.routers.api.tls.domains[0].main": "x.example.com",
		})
		if len(routes) != 1 || routes[0].Rule != "Host(`api.example.com`)" {
			t.Fatalf("routesFromLabels() = %+v, want the rule only", routes)
		}
	})

	t.Run("other protocols are ignored", func(t *testing.T) {
		routes := routesFromLabels(map[string]string{
			"traefik.udp.routers.dns.rule": "HostSNI(`dns.example.com`)",
		})
		if routes != nil {
			t.Errorf("routesFromLabels() = %+v, want none", routes)
		}
	})

	t.Run("http wins over tcp for the same name", func(t *testing.T) {
		routes := routesFromLabels(map[string]string{
			"traefik.http.routers.api.rule": "Host(`api.example.com`)",
			"traefik.tcp.routers.api.rule":  "HostSNI(`api.example.com`)",
		})
		if len(routes) != 1 || routes[0].Kind != "http" {
			t.Errorf("routesFromLabels() = %+v, want the HTTP route", routes)
		}
	})

	t.Run("router names with dashes and underscores", func(t *testing.T) {
		routes := routesFromLabels(map[string]string{
			"traefik.http.routers.my_api-v2.rule": "Host(`api.example.com`)",
		})
		if len(routes) != 1 || routes[0].Name != "my_api-v2@docker" {
			t.Errorf("routesFromLabels() = %+v, want my_api-v2@docker", routes)
		}
	})

	t.Run("entrypoints list", func(t *testing.T) {
		routes := routesFromLabels(map[string]string{
			"traefik.http.routers.api.rule":        "Host(`api.example.com`)",
			"traefik.http.routers.api.entrypoints": "web, websecure",
		})
		want := []string{"web", "websecure"}
		if len(routes) != 1 || !reflect.DeepEqual(routes[0].Entrypoints, want) {
			t.Fatalf("routesFromLabels() = %+v, want entrypoints %v", routes, want)
		}
	})
}

func TestDefaultRouterApp(t *testing.T) {
	app := &App{Name: "shop"}
	containers := []*Container{
		{ID: "a", App: app, Labels: map[string]string{
			labelComposeService: "api",
			labelComposeProject: "shop",
		}},
		{ID: "b", App: &App{Name: "standalone"}, Labels: map[string]string{}},
	}
	tests := []struct {
		name       string
		routerName string
		wantApp    string
		wantOK     bool
	}{
		{name: "default docker router", routerName: "api-shop@docker", wantApp: "shop", wantOK: true},
		{name: "file provider", routerName: "api-shop@file"},
		{name: "internal router", routerName: "api@internal"},
		{name: "unknown service", routerName: "web-shop@docker"},
		{name: "reversed name", routerName: "shop-api@docker"},
		{name: "no provider", routerName: "api-shop"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := defaultRouterApp(containers, tt.routerName)
			if ok != tt.wantOK {
				t.Fatalf("defaultRouterApp(%q) ok = %v, want %v", tt.routerName, ok, tt.wantOK)
			}
			if ok && got.Name != tt.wantApp {
				t.Errorf("defaultRouterApp(%q) = %q, want %q", tt.routerName, got.Name, tt.wantApp)
			}
		})
	}
}

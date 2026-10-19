package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The system D-Bus disconnects users that the host's user database does not
// know, so the container must run as a user every host has. These tests keep
// the image, the production compose file and the documentation agreeing.

// readRepoFile reads a file relative to the repository root.
func readRepoFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("../../" + name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

func TestContainerRunsAsNobody(t *testing.T) {
	const want = "65534:65534"
	dockerfile := regexp.MustCompile(`(?m)^USER (\S+)$`).FindStringSubmatch(readRepoFile(t, "Dockerfile"))
	if dockerfile == nil || dockerfile[1] != want {
		t.Errorf("Dockerfile USER = %v, want %s", dockerfile, want)
	}
	compose := regexp.MustCompile(`(?m)^\s+user: "([^"]+)"$`).FindStringSubmatch(readRepoFile(t, "deploy/docker-compose.yml"))
	if compose == nil || compose[1] != want {
		t.Errorf("deploy/docker-compose.yml user = %v, want %s", compose, want)
	}
	if got := readRepoFile(t, "docs/deployment.md"); strings.Contains(got, "65532") {
		t.Error("docs/deployment.md still names uid 65532")
	}
}

func TestProductionComposeMountsTheSystemBus(t *testing.T) {
	compose := readRepoFile(t, "deploy/docker-compose.yml")
	if !strings.Contains(compose, "/run/dbus/system_bus_socket:/run/dbus/system_bus_socket:ro") {
		t.Error("deploy/docker-compose.yml does not mount the system bus socket")
	}
	env := readRepoFile(t, "deploy/.env.example")
	if !regexp.MustCompile(`(?m)^RAEDAR_SYSTEMD=auto$`).MatchString(env) {
		t.Error("deploy/.env.example must not make systemd mandatory; RAEDAR_SYSTEMD should be auto")
	}
}

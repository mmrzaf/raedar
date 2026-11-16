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
	compose := regexp.MustCompile(`(?m)^\s+user: "([^"]+)"$`).FindStringSubmatch(readRepoFile(t, "deploy/compose.yaml"))
	if compose == nil || compose[1] != want {
		t.Errorf("deploy/compose.yaml user = %v, want %s", compose, want)
	}
	if got := readRepoFile(t, "docs/deployment.md"); strings.Contains(got, "65532") {
		t.Error("docs/deployment.md still names uid 65532")
	}
}

func TestHostOverrideMountsTheSystemBusAndKeepsSystemdOptional(t *testing.T) {
	compose := readRepoFile(t, "deploy/compose.host.yaml")
	if !strings.Contains(compose, "/run/dbus/system_bus_socket:/run/dbus/system_bus_socket:ro") {
		t.Error("deploy/compose.host.yaml does not mount the system bus socket")
	}
	if !regexp.MustCompile(`(?m)^\s+RAEDAR_SYSTEMD: auto$`).MatchString(compose) {
		t.Error("deploy/compose.host.yaml must not make systemd mandatory; RAEDAR_SYSTEMD should be auto")
	}
}

// TestDeployFilesAreNotCoupledToAnOperator keeps private infrastructure
// names and absolute host paths out of the files operators copy.
func TestDeployFilesAreNotCoupledToAnOperator(t *testing.T) {
	for _, name := range []string{"deploy/compose.yaml", "deploy/compose.host.yaml", "deploy/.env.example", "deploy/raedar.env.example", "deploy/README.md"} {
		text := strings.ToLower(readRepoFile(t, name))
		for _, banned := range []string{"waiotech", "arvancloud", "admin-auth@file", "/srv/", "zld_"} {
			if strings.Contains(text, banned) {
				t.Errorf("%s contains %q", name, banned)
			}
		}
	}
}

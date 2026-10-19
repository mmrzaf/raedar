package discovery

import (
	"strconv"
	"strings"
)

// identity is where a container belongs, as far as its name and labels say.
// The model builder reads it from an inspect response and the event handler
// from the attributes of a Docker event, so both place a container the same
// way.
type identity struct {
	// App is the raedar.app label, the Compose project, or the container name.
	App string
	// SlotKey is "service #number" for Compose containers, otherwise the
	// container name.
	SlotKey string
	// Service is the Compose service name, or the container name.
	Service string
	// Replica is the Compose container number, 1 for standalone containers.
	Replica int
	// Ignored reports whether the container carries raedar.ignore=true.
	Ignored bool
	// OneOff reports whether the container was started by docker compose run.
	OneOff bool
}

// identify reads the identity of a container from its name and labels.
func identify(name string, labels map[string]string) identity {
	id := identity{
		App:     name,
		SlotKey: name,
		Service: name,
		Replica: 1,
		Ignored: isTrue(labels[labelIgnore]),
		OneOff:  isTrue(labels[labelComposeOneOff]),
	}
	if app := strings.TrimSpace(labels[labelApp]); app != "" {
		id.App = app
	} else if project := strings.TrimSpace(labels[labelComposeProject]); project != "" {
		id.App = project
	}
	service := strings.TrimSpace(labels[labelComposeService])
	number := strings.TrimSpace(labels[labelComposeNumber])
	if service != "" {
		id.Service = service
		if n, err := strconv.Atoi(number); err == nil && n > 0 {
			id.Replica = n
		}
		if number != "" {
			id.SlotKey = service + " #" + number
		}
	}
	return id
}

// Tracked reports whether the container belongs in the model. Containers that
// are ignored, and one-off containers such as migrations or shells, are not
// monitored: they get no slot, no samples, no events and no alerts.
func (i identity) Tracked() bool {
	return !i.Ignored && !i.OneOff
}

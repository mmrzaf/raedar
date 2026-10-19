package discovery

import (
	"sort"
	"time"

	"github.com/mmrzaf/raedar/internal/docker"
)

// intentionalWindow is how long a kill or stop event makes a die event
// intentional.
const intentionalWindow = 30 * time.Second

// stopGrace is how long a die event waits for a stop that follows it.
const stopGrace = time.Second

// LifecycleEvent is one event derived from the Docker event stream.
type LifecycleEvent struct {
	// Kind is crashed, stopped, started, health or removed.
	Kind string
	// ContainerID is the Docker container ID.
	ContainerID string
	// Name is the container name.
	Name string
	// ImageRef is the container's image reference.
	ImageRef string
	// ExitCode is the exit code of a crashed container, -1 otherwise.
	ExitCode int
	// OOMKilled reports whether a crash was caused by the OOM killer.
	OOMKilled bool
	// Health is the new health status of a health event.
	Health string
	// Time is when the event happened.
	Time time.Time
}

// pendingDie is a die event that waits for a stop that follows it.
type pendingDie struct {
	event    docker.Event
	deadline time.Time
}

// classifier derives container lifecycle events from the Docker event stream.
// Its state is owned by one goroutine.
type classifier struct {
	grace       time.Duration
	intentional map[string]time.Time
	oom         map[string]time.Time
	pending     map[string]pendingDie
}

// newClassifier returns an empty classifier.
func newClassifier() *classifier {
	return &classifier{
		grace:       stopGrace,
		intentional: make(map[string]time.Time),
		oom:         make(map[string]time.Time),
		pending:     make(map[string]pendingDie),
	}
}

// Feed processes one Docker event and returns the lifecycle events it
// produces. A die event waits for its grace period before it is classified,
// in case a stop follows it.
func (c *classifier) Feed(event docker.Event) []LifecycleEvent {
	c.prune(event.Time)
	switch event.Action {
	case "kill", "stop":
		c.intentional[event.ID] = event.Time
		if pending, ok := c.pending[event.ID]; ok {
			delete(c.pending, event.ID)
			return []LifecycleEvent{stoppedEvent(pending.event)}
		}
	case "die":
		if recent(c.intentional[event.ID], event.Time) {
			return []LifecycleEvent{stoppedEvent(event)}
		}
		c.pending[event.ID] = pendingDie{event: event, deadline: event.Time.Add(c.grace)}
	case "start", "restart":
		return []LifecycleEvent{{
			Kind:        "started",
			ContainerID: event.ID,
			Name:        event.Name,
			ImageRef:    event.Image,
			Time:        event.Time,
		}}
	case "health_status":
		if event.Health == "" {
			return nil
		}
		return []LifecycleEvent{{
			Kind:        "health",
			ContainerID: event.ID,
			Name:        event.Name,
			ImageRef:    event.Image,
			Health:      event.Health,
			Time:        event.Time,
		}}
	case "oom":
		c.oom[event.ID] = event.Time
	case "destroy":
		delete(c.pending, event.ID)
		return []LifecycleEvent{{
			Kind:        "removed",
			ContainerID: event.ID,
			Name:        event.Name,
			ImageRef:    event.Image,
			Time:        event.Time,
		}}
	}
	return nil
}

// Flush returns the events of the die events whose grace period passed at
// now.
func (c *classifier) Flush(now time.Time) []LifecycleEvent {
	var events []LifecycleEvent
	for id, pending := range c.pending {
		if pending.deadline.After(now) {
			continue
		}
		delete(c.pending, id)
		if event, ok := c.classifyDie(pending.event); ok {
			events = append(events, event)
		}
	}
	sort.Slice(events, func(i, j int) bool {
		if !events[i].Time.Equal(events[j].Time) {
			return events[i].Time.Before(events[j].Time)
		}
		return events[i].ContainerID < events[j].ContainerID
	})
	return events
}

// classifyDie returns the event of a die that was not intentional: crashed
// for a non-zero exit code, nothing for exit code zero or an unknown code.
func (c *classifier) classifyDie(event docker.Event) (LifecycleEvent, bool) {
	if event.ExitCode <= 0 {
		return LifecycleEvent{}, false
	}
	return LifecycleEvent{
		Kind:        "crashed",
		ContainerID: event.ID,
		Name:        event.Name,
		ImageRef:    event.Image,
		ExitCode:    event.ExitCode,
		OOMKilled:   recent(c.oom[event.ID], event.Time),
		Time:        event.Time,
	}, true
}

// stoppedEvent returns the stopped event of an intentional die.
func stoppedEvent(die docker.Event) LifecycleEvent {
	return LifecycleEvent{
		Kind:        "stopped",
		ContainerID: die.ID,
		Name:        die.Name,
		ImageRef:    die.Image,
		ExitCode:    die.ExitCode,
		Time:        die.Time,
	}
}

// prune drops entries that can no longer matter.
func (c *classifier) prune(now time.Time) {
	for id, at := range c.intentional {
		if now.Sub(at) > intentionalWindow {
			delete(c.intentional, id)
		}
	}
	for id, at := range c.oom {
		if now.Sub(at) > intentionalWindow {
			delete(c.oom, id)
		}
	}
}

// recent reports whether at is set and not older than the intentional window
// before reference.
func recent(at, reference time.Time) bool {
	return !at.IsZero() && !at.Before(reference.Add(-intentionalWindow))
}

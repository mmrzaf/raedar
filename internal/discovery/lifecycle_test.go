package discovery

import (
	"reflect"
	"testing"
	"time"

	"github.com/mmrzaf/raedar/internal/docker"
)

// dockerEvent builds one container event for the classifier tests.
func dockerEvent(action string, at time.Time) docker.Event {
	return docker.Event{
		Action: action,
		ID:     "c1",
		Name:   "shop-api-1",
		Image:  "shop/api:1.0",
		Time:   at,
	}
}

// withExitCode returns a die event with an exit code.
func withExitCode(event docker.Event, code int) docker.Event {
	event.ExitCode = code
	return event
}

func TestClassifierSequences(t *testing.T) {
	base := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	at := func(offset time.Duration) time.Time { return base.Add(offset) }
	tests := []struct {
		name    string
		events  []docker.Event
		flushAt time.Time
		flushed bool
		want    []LifecycleEvent
	}{
		{
			name: "kill then die is intentional",
			events: []docker.Event{
				dockerEvent("kill", at(0)),
				withExitCode(dockerEvent("die", at(time.Second)), 137),
			},
			want: []LifecycleEvent{{Kind: "stopped", ExitCode: 137, Time: at(time.Second)}},
		},
		{
			name: "stop then die is intentional",
			events: []docker.Event{
				dockerEvent("stop", at(0)),
				withExitCode(dockerEvent("die", at(2*time.Second)), 143),
			},
			want: []LifecycleEvent{{Kind: "stopped", ExitCode: 143, Time: at(2 * time.Second)}},
		},
		{
			name: "die then stop within the grace period",
			events: []docker.Event{
				withExitCode(dockerEvent("die", at(time.Second)), 143),
				dockerEvent("stop", at(2*time.Second)),
			},
			want: []LifecycleEvent{{Kind: "stopped", ExitCode: 143, Time: at(time.Second)}},
		},
		{
			name:    "die alone is a crash",
			events:  []docker.Event{withExitCode(dockerEvent("die", at(time.Second)), 1)},
			flushAt: at(5 * time.Second),
			flushed: true,
			want:    []LifecycleEvent{{Kind: "crashed", ExitCode: 1, Time: at(time.Second)}},
		},
		{
			name:    "die with exit code zero produces nothing",
			events:  []docker.Event{withExitCode(dockerEvent("die", at(time.Second)), 0)},
			flushAt: at(5 * time.Second),
			flushed: true,
		},
		{
			name:    "unknown exit code produces nothing",
			events:  []docker.Event{withExitCode(dockerEvent("die", at(time.Second)), -1)},
			flushAt: at(5 * time.Second),
			flushed: true,
		},
		{
			name: "kill outside the window is a crash",
			events: []docker.Event{
				dockerEvent("kill", at(0)),
				withExitCode(dockerEvent("die", at(31*time.Second)), 137),
			},
			flushAt: at(40 * time.Second),
			flushed: true,
			want:    []LifecycleEvent{{Kind: "crashed", ExitCode: 137, Time: at(31 * time.Second)}},
		},
		{
			name: "oom makes the crash out of memory",
			events: []docker.Event{
				dockerEvent("oom", at(0)),
				withExitCode(dockerEvent("die", at(time.Second)), 137),
			},
			flushAt: at(5 * time.Second),
			flushed: true,
			want:    []LifecycleEvent{{Kind: "crashed", ExitCode: 137, OOMKilled: true, Time: at(time.Second)}},
		},
		{
			name:   "start",
			events: []docker.Event{dockerEvent("start", at(0))},
			want:   []LifecycleEvent{{Kind: "started", Time: at(0)}},
		},
		{
			name:   "restart is recorded once, by its start",
			events: []docker.Event{dockerEvent("start", at(0)), dockerEvent("restart", at(0))},
			want:   []LifecycleEvent{{Kind: "started", Time: at(0)}},
		},
		{
			name: "health status",
			events: []docker.Event{func() docker.Event {
				event := dockerEvent("health_status", at(0))
				event.Health = "unhealthy"
				return event
			}()},
			want: []LifecycleEvent{{Kind: "health", Health: "unhealthy", Time: at(0)}},
		},
		{
			name:   "destroy",
			events: []docker.Event{dockerEvent("destroy", at(0))},
			want:   []LifecycleEvent{{Kind: "removed", Time: at(0)}},
		},
		{
			name: "destroy cancels a pending crash",
			events: []docker.Event{
				withExitCode(dockerEvent("die", at(time.Second)), 1),
				dockerEvent("destroy", at(2*time.Second)),
			},
			flushAt: at(5 * time.Second),
			flushed: true,
			want:    []LifecycleEvent{{Kind: "removed", Time: at(2 * time.Second)}},
		},
		{
			name: "create and rename produce nothing",
			events: []docker.Event{
				dockerEvent("create", at(0)),
				dockerEvent("rename", at(time.Second)),
			},
		},
		{
			name:    "die before the grace period is not classified",
			events:  []docker.Event{withExitCode(dockerEvent("die", at(time.Second)), 1)},
			flushAt: at(time.Second),
			flushed: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newClassifier()
			var produced []LifecycleEvent
			for _, event := range tt.events {
				produced = append(produced, c.Feed(event)...)
			}
			if tt.flushed {
				produced = append(produced, c.Flush(tt.flushAt)...)
			}
			if len(produced) != len(tt.want) {
				t.Fatalf("events = %+v, want %+v", produced, tt.want)
			}
			for i, want := range tt.want {
				got := produced[i]
				if got.Kind != want.Kind || got.ExitCode != want.ExitCode || got.OOMKilled != want.OOMKilled ||
					got.Health != want.Health || !got.Time.Equal(want.Time) {
					t.Errorf("event %d = %+v, want %+v", i, got, want)
				}
			}
		})
	}
}

func TestClassifierFlushOrderIsStable(t *testing.T) {
	base := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	c := newClassifier()
	first := withExitCode(dockerEvent("die", base), 1)
	first.ID = "c2"
	second := withExitCode(dockerEvent("die", base), 1)
	second.ID = "c1"
	c.Feed(first)
	c.Feed(second)
	events := c.Flush(base.Add(time.Minute))
	if len(events) != 2 {
		t.Fatalf("events = %+v, want 2", events)
	}
	if events[0].ContainerID != "c1" || events[1].ContainerID != "c2" {
		t.Errorf("events = %+v, want c1 before c2", events)
	}
	if !reflect.DeepEqual(c.pending, map[string]pendingDie{}) {
		t.Errorf("pending = %+v, want empty", c.pending)
	}
}

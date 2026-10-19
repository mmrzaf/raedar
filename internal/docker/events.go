package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Event is one container event from the Docker event stream.
type Event struct {
	// Action is what happened: create, start, kill, die, stop, destroy, oom,
	// health_status, restart or rename.
	Action string
	// ID is the container ID.
	ID string
	// Name is the container name.
	Name string
	// Image is the image reference the container runs.
	Image string
	// ExitCode is the exit code of a die event, -1 for other events.
	ExitCode int
	// Health is the new health status of a health_status event.
	Health string
	// Time is when the event happened.
	Time time.Time
	// Attributes are the event's attributes, including container labels and
	// the Compose project and service.
	Attributes map[string]string
}

// event is one event as the daemon writes it.
type event struct {
	Status   string `json:"status"`
	Action   string `json:"Action"`
	Type     string `json:"Type"`
	ID       string `json:"id"`
	From     string `json:"from"`
	Time     int64  `json:"time"`
	TimeNano int64  `json:"timeNano"`
	Actor    struct {
		ID         string            `json:"ID"`
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
}

// convert turns one daemon event into an Event.
func (e event) convert() Event {
	action := e.Action
	if action == "" {
		action = e.Status
	}
	health := ""
	if rest, ok := strings.CutPrefix(action, "health_status:"); ok {
		health = strings.TrimSpace(rest)
		action = "health_status"
	}
	image := e.From
	if image == "" {
		image = e.Actor.Attributes["image"]
	}
	exitCode := -1
	if raw, ok := e.Actor.Attributes["exitCode"]; ok {
		if code, err := strconv.Atoi(raw); err == nil {
			exitCode = code
		}
	}
	when := time.Unix(e.Time, 0).UTC()
	if e.TimeNano > 0 {
		when = time.Unix(0, e.TimeNano).UTC()
	}
	return Event{
		Action:     action,
		ID:         e.ID,
		Name:       e.Actor.Attributes["name"],
		Image:      image,
		ExitCode:   exitCode,
		Health:     health,
		Time:       when,
		Attributes: e.Actor.Attributes,
	}
}

// Events reads the container event stream and calls handle for every event
// until the context is done or the daemon closes the stream. A stream that
// ends without an error returns nil; the caller reconnects.
func (c *Client) Events(ctx context.Context, handle func(Event)) error {
	query := url.Values{"filters": {`{"type":["container"]}`}}
	resp, err := c.get(ctx, "/events", query, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	decoder := json.NewDecoder(resp.Body)
	for {
		var next event
		err := decoder.Decode(&next)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("docker: read events: %w", err)
		}
		handle(next.convert())
	}
}

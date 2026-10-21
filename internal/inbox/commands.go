package inbox

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mmrzaf/raedar/internal/config"
)

// Command is one parsed operator command. Unknown text produces an empty
// command and ErrUnknownCmd, which the caller answers with help text.
type Command struct {
	// Name is the verb: help, status, ack, mute, unmute.
	Name string
	// Args is the rest of the text, split by whitespace. The dispatcher
	// decides how to parse them per verb.
	Args []string
	// Raw is the original message text.
	Raw string
}

// ErrUnknownCmd signals that the message was not a Raedar command; the
// caller answers with help text or silence.
var ErrUnknownCmd = errors.New("not a command")

// Parse reads an operator's message and returns the matched command, or
// ErrUnknownCmd when the text does not start with a known verb. Reply-to
// messages can begin with `mute <duration>` etc. without a leading slash
// for ergonomic reasons; the parser accepts both forms.
func Parse(text string) (Command, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return Command{}, ErrUnknownCmd
	}
	fields := strings.Fields(trimmed)
	first := fields[0]
	if strings.HasPrefix(first, "/") {
		first = strings.TrimPrefix(first, "/")
		if at := strings.Index(first, "@"); at >= 0 {
			first = first[:at]
		}
	}
	switch first {
	case "help":
		return Command{Name: "help", Args: fields[1:], Raw: trimmed}, nil
	case "status":
		return Command{Name: "status", Args: fields[1:], Raw: trimmed}, nil
	case "ack":
		return Command{Name: "ack", Args: fields[1:], Raw: trimmed}, nil
	case "mute":
		return Command{Name: "mute", Args: fields[1:], Raw: trimmed}, nil
	case "unmute":
		return Command{Name: "unmute", Args: fields[1:], Raw: trimmed}, nil
	}
	return Command{}, ErrUnknownCmd
}

// muteUsage is the reply to a mute command the parser cannot read.
const muteUsage = "usage: mute <rule> <subject> <duration>  ·  mute <rule> <subject> until <RFC3339>  ·  " +
	"reply mute 2h to a paged message"

// ParseMuteArgs parses the args of a `mute` command:
//
//	mute <rule> <subject> <duration>
//	mute <rule> <subject> until <RFC3339>
//	mute <duration>                  (a reply to a paged message)
//	mute until <RFC3339>             (a reply to a paged message)
//
// The short forms return an empty rule and subject; the caller resolves them
// from the replied-to message. A duration is like 30s, 5m, 2h or 14d. Relative
// end times are anchored to now.
func ParseMuteArgs(args []string, now time.Time) (rule, subject string, endsAt time.Time, err error) {
	switch {
	case len(args) == 1:
		endsAt, err = parseMuteEnd(args, now)
	case len(args) == 2 && strings.EqualFold(args[0], "until"):
		endsAt, err = parseMuteEnd(args, now)
	case len(args) == 3 || len(args) == 4:
		rule, subject = args[0], args[1]
		endsAt, err = parseMuteEnd(args[2:], now)
	default:
		err = errors.New(muteUsage)
	}
	return rule, subject, endsAt, err
}

// parseMuteEnd reads the end of a mute: a duration, or `until <RFC3339>`.
func parseMuteEnd(args []string, now time.Time) (time.Time, error) {
	if strings.EqualFold(args[0], "until") {
		if len(args) != 2 {
			return time.Time{}, errors.New("usage: until <RFC3339>, for example until 2026-10-21T08:00:00Z")
		}
		at, err := time.Parse(time.RFC3339, args[1])
		if err != nil {
			return time.Time{}, fmt.Errorf("until must be RFC3339, for example 2026-10-21T08:00:00Z: %w", err)
		}
		return at, nil
	}
	if len(args) != 1 {
		return time.Time{}, errors.New(muteUsage)
	}
	d, err := config.ParseDuration(args[0])
	if err != nil {
		return time.Time{}, fmt.Errorf("duration must be like 30s, 5m, 2h or 14d: %w", err)
	}
	return now.Add(d), nil
}

// ParseAckArgs expects an incident id.
func ParseAckArgs(args []string) (int64, error) {
	if len(args) != 1 {
		return 0, errors.New("usage: ack <incident-id>")
	}
	id, err := parseInt64(args[0])
	if err != nil {
		return 0, fmt.Errorf("incident id must be a number: %w", err)
	}
	return id, nil
}

// ParseUnmuteArgs expects the id of the silence to remove.
func ParseUnmuteArgs(args []string) (int64, error) {
	if len(args) != 1 {
		return 0, errors.New("usage: unmute <silence-id>")
	}
	id, err := parseInt64(args[0])
	if err != nil {
		return 0, fmt.Errorf("silence id must be a number: %w", err)
	}
	return id, nil
}

func parseInt64(s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, errors.New("not a positive number")
	}
	return n, nil
}

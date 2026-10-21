package inbox

import (
	"errors"
	"fmt"
	"strings"
	"time"
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

// ParseMuteArgs parses the args of a `mute` command. The arg shape is:
//
//	mute <rule> <subject> <duration|until>
//	mute <rule> <subject> until <RFC3339>
//
// and the silent form (operator just types `mute 2h` or `mute until 14:00`)
// when the message was a reply to a paged message; in that case rule and
// subject are nil and the caller resolves them from the replied-to delivery.
//
// A duration is one of "30s", "5m", "2h", "14d", or any Go duration the
// `time.ParseDuration` accepts. A leading word `until` requests an RFC3339
// end-time.
//
// End times are anchored to nowFunc so tests can fix the clock.
func ParseMuteArgs(args []string) (rule, subject string, endsAt time.Time, err error) {
	// Silent form: just `mute 2h` or `mute until 14:00` (subject inferred
	// from the replied-to message).
	if len(args) == 1 {
		rule, subject, endsAt, err = parseMuteDuration(args[0])
		return
	}
	if len(args) == 2 && strings.EqualFold(args[0], "until") {
		rule, subject, endsAt, err = parseMuteUntil(args[1])
		return
	}
	// Full form: rule subject <duration|until X>
	if len(args) < 3 {
		err = errors.New("usage: mute <rule> <subject> <duration>  ·  mute <rule> <subject> until <RFC3339>  ·  reply `mute 2h` to any paged message")
		return
	}
	rule = args[0]
	subject = args[1]
	if strings.EqualFold(args[2], "until") {
		if len(args) < 4 {
			err = errors.New("usage: mute <rule> <subject> until <RFC3339>")
			return
		}
		t, parseErr := time.Parse(time.RFC3339, args[3])
		if parseErr != nil {
			err = fmt.Errorf("until must be RFC3339, for example 2026-10-21T08:00:00Z: %w", parseErr)
			return
		}
		endsAt = t
		return
	}
	d, parseErr := time.ParseDuration(args[2])
	if parseErr != nil || d <= 0 {
		err = fmt.Errorf("duration must be a Go duration like 30s, 5m, 2h or 14d: %v", parseErr)
		return
	}
	endsAt = nowFunc().Add(d)
	return
}

func parseMuteDuration(arg string) (rule, subject string, endsAt time.Time, err error) {
	if strings.EqualFold(arg, "until") {
		err = errors.New("until takes an RFC3339 time, for example `mute until 2026-10-21T08:00:00Z`")
		return
	}
	d, parseErr := time.ParseDuration(arg)
	if parseErr != nil || d <= 0 {
		err = fmt.Errorf("duration must be a Go duration like 30s, 5m, 2h or 14d: %v", parseErr)
		return
	}
	endsAt = nowFunc().Add(d)
	return
}

func parseMuteUntil(arg string) (rule, subject string, endsAt time.Time, err error) {
	t, parseErr := time.Parse(time.RFC3339, arg)
	if parseErr != nil {
		err = fmt.Errorf("until must be RFC3339, for example 2026-10-21T08:00:00Z: %w", parseErr)
		return
	}
	endsAt = t
	return
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
	var n int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errors.New("not a number")
		}
		n = n*10 + int64(r-'0')
	}
	return n, nil
}

// nowFunc lets tests fix the clock. Production code paths always use
// time.Now via the closure below.
var nowFunc = func() time.Time { return time.Now() }

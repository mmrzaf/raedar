package inbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mmrzaf/raedar/internal/store"
)

// Store is the slice of *store.Store the dispatcher uses.
type Store interface {
	RecordProcessedUpdate(ctx context.Context, updateID int64) (bool, error)
	CreateSilence(ctx context.Context, silence store.Silence) (int64, error)
	DeleteSilence(ctx context.Context, id int64) (bool, error)
	AckIncident(ctx context.Context, id int64, at time.Time, by string) (bool, error)
	IncidentByID(ctx context.Context, id int64) (*store.Incident, error)
	OpenIncidents(ctx context.Context) (map[store.IncidentKey]store.Incident, error)
	LookupDeliveryByTelegramMessageID(ctx context.Context, messageID int64) (*store.Delivery, error)
	ActiveSilences(ctx context.Context, now time.Time) ([]store.Silence, error)
}

// notifier is what the dispatcher needs from the pager. Phase 2 wires
// enough to acknowledge; the mute reply is enough to suppress pages
// until the silence expires.
type notifier interface {
	SendReply(ctx context.Context, chatID int64, text string) error
}

// Dispatcher parses commands and applies the matching side effects.
type Dispatcher struct {
	Store    Store
	Notifier notifier
	Logger   *slog.Logger
	Host     string
	// AckBy is the identifier written to incidents.acked_by when an operator
	// acks through Telegram. Typically "telegram:<user-id>".
	AckBy func(chatID, userID int64) string
}

// Reply is one Telegram reply the dispatcher wants to send. The inbox
// goroutine writes these to the bot.
type Reply struct {
	ChatID int64
	Text   string
}

// Result is one parsed command's outcome.
type Result struct {
	Reply Reply
}

// Execute is the dispatcher. It parses the command, applies state, and
// returns the reply to send back. The Reply in Result.Text is HTML;
// the inbox Source passes it straight to the Telegram channel.
func (d *Dispatcher) Execute(ctx context.Context, msg Message) (Result, error) {
	cmd, err := Parse(msg.Text)
	if err != nil {
		// Non-commands are silently ignored; the bot is only an operator
		// tool, not a chat-bot.
		if errors.Is(err, ErrUnknownCmd) {
			return Result{}, nil
		}
		return Result{}, err
	}
	switch cmd.Name {
	case "help":
		return d.reply(msg, helpText()), nil
	case "status":
		return d.status(ctx, msg)
	case "ack":
		return d.ack(ctx, msg, cmd)
	case "mute":
		return d.mute(ctx, msg, cmd)
	case "unmute":
		return d.unmute(ctx, msg, cmd)
	}
	return Result{}, fmt.Errorf("unsupported command %q", cmd.Name)
}

// Send the reply to Telegram via the configured notifier. Called by the
// Source loop after Execute returns. Returns an error when no reply
// is configured or when the channel rejects the send.
func (d *Dispatcher) SendReply(ctx context.Context, result Result) error {
	if result.Reply.Text == "" {
		return nil
	}
	if d.Notifier == nil {
		return fmt.Errorf("no notifier configured for replies")
	}
	return d.Notifier.SendReply(ctx, result.Reply.ChatID, result.Reply.Text)
}

func (d *Dispatcher) reply(msg Message, text string) Result {
	return Result{Reply: Reply{ChatID: msg.ChatID, Text: text}}
}

func (d *Dispatcher) status(ctx context.Context, msg Message) (Result, error) {
	open, err := d.Store.OpenIncidents(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("read open incidents: %w", err)
	}
	now := time.Now()
	active, err := d.Store.ActiveSilences(ctx, now)
	if err != nil {
		return Result{}, fmt.Errorf("read silences: %w", err)
	}
	text := formatStatus(d.Host, open, active, now)
	return d.reply(msg, text), nil
}

func (d *Dispatcher) ack(ctx context.Context, msg Message, cmd Command) (Result, error) {
	id, err := ParseAckArgs(cmd.Args)
	if err != nil {
		return d.reply(msg, err.Error()), nil
	}
	incident, err := d.Store.IncidentByID(ctx, id)
	if err != nil {
		return d.reply(msg, fmt.Sprintf("could not read incident %d: %v", id, err)), nil
	}
	if incident == nil {
		return d.reply(msg, fmt.Sprintf("incident %d does not exist", id)), nil
	}
	if incident.ResolvedAt != nil {
		return d.reply(msg, fmt.Sprintf("incident %d is already resolved", id)), nil
	}
	if incident.Acked() {
		return d.reply(msg, fmt.Sprintf("incident %d was already acked", id)), nil
	}
	acked, err := d.Store.AckIncident(ctx, id, time.Now(), d.ackBy(msg))
	if err != nil {
		return d.reply(msg, fmt.Sprintf("could not ack: %v", err)), nil
	}
	if !acked {
		return d.reply(msg, fmt.Sprintf("incident %d was already acked by another operator", id)), nil
	}
	return d.reply(msg, fmt.Sprintf("acked incident %d (%s on %s)",
		id, incident.Rule, incident.Subject)), nil
}

func (d *Dispatcher) mute(ctx context.Context, msg Message, cmd Command) (Result, error) {
	rule, subject, endsAt, err := ParseMuteArgs(cmd.Args)
	if err != nil {
		return d.reply(msg, err.Error()), nil
	}
	// Silent form: resolve (rule, subject) from the reply-to message.
	if rule == "" {
		if msg.ReplyToUpdateID == 0 {
			return d.reply(msg, "send `mute <rule> <subject> <duration>` or reply with `mute 2h` to a paged message"), nil
		}
		delivery, err := d.Store.LookupDeliveryByTelegramMessageID(ctx, msg.ReplyToUpdateID)
		if err != nil {
			return d.reply(msg, fmt.Sprintf("could not resolve the replied-to message: %v", err)), nil
		}
		if delivery == nil {
			return d.reply(msg, "the message you replied to is not a Raedar page I can mute"), nil
		}
		if delivery.IncidentID == nil {
			return d.reply(msg, "the message you replied to is not tied to a single incident"), nil
		}
		incident, err := d.Store.IncidentByID(ctx, *delivery.IncidentID)
		if err != nil || incident == nil {
			return d.reply(msg, "the original incident is gone"), nil
		}
		rule = incident.Rule
		subject = incident.Subject
	}
	if endsAt.Before(time.Now()) {
		return d.reply(msg, "the requested end time is already in the past"), nil
	}
	id, err := d.Store.CreateSilence(ctx, store.Silence{
		Kind:      store.SilenceOneShotMute,
		Rule:      stringPtr(rule),
		Subject:   stringPtr(subject),
		Reason:    d.ackBy(msg),
		StartsAt:  time.Now(),
		EndsAt:    endsAt,
		CreatedBy: stringPtr("telegram"),
		Comment:   stringPtr(msg.Text),
	})
	if err != nil {
		return d.reply(msg, fmt.Sprintf("could not record the silence: %v", err)), nil
	}
	return d.reply(msg, fmt.Sprintf("muted %s on %s until %s (silence #%d)",
		rule, subject, endsAt.In(time.Local).Format(time.RFC1123), id)), nil
}

func (d *Dispatcher) unmute(ctx context.Context, msg Message, cmd Command) (Result, error) {
	id, err := ParseUnmuteArgs(cmd.Args)
	if err != nil {
		return d.reply(msg, err.Error()), nil
	}
	deleted, err := d.Store.DeleteSilence(ctx, id)
	if err != nil {
		return d.reply(msg, fmt.Sprintf("could not delete silence %d: %v", id, err)), nil
	}
	if !deleted {
		return d.reply(msg, fmt.Sprintf("silence %d does not exist (or already expired)", id)), nil
	}
	return d.reply(msg, fmt.Sprintf("removed silence #%d", id)), nil
}

// ackBy returns the identifier the dispatcher writes to
// incidents.acked_by. Defaults to "telegram:<user-id>".
func (d *Dispatcher) ackBy(msg Message) string {
	if d.AckBy != nil {
		return d.AckBy(msg.ChatID, msg.UserID)
	}
	return fmt.Sprintf("telegram:%d", msg.UserID)
}

func stringPtr(s string) *string { return &s }

// helpText is the response to `help`. The Telegram-style Markdown is
// limited to code blocks and a few inline markers; we keep it minimal.
func helpText() string {
	return strings.TrimSpace(`
Raedar commands

*help* — this message
*status* — open incidents and active silences
*ack <id>* — mark an incident acked; all its messages stop
*mute <rule> <subject> <duration>* — silence one (rule, subject)
*mute <rule> <subject> until <RFC3339>* — silence until a timestamp
*reply "mute 2h"* — silence the (rule, subject) of a paged message
*reply "mute until 14:00"* — same, with an explicit end time
*unmute <silence-id>* — remove a silence

Durations are Go durations: 30s, 5m, 2h, 14d.
`)
}

func formatStatus(host string, open map[store.IncidentKey]store.Incident, silences []store.Silence, now time.Time) string {
	critical := 0
	warning := 0
	for _, incident := range open {
		if incident.Severity == store.SeverityCritical {
			critical++
		} else {
			warning++
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s · %d open (%d critical, %d warning) · %d active silences\n",
		host, len(open), critical, warning, len(silences))
	for _, silence := range silences {
		rule := "any"
		if silence.Rule != nil {
			rule = *silence.Rule
		}
		subject := "any"
		if silence.Subject != nil {
			subject = *silence.Subject
		}
		remaining := silence.EndsAt.Sub(now).Round(time.Minute)
		fmt.Fprintf(&sb, "  #%d  %s  %s on %s  (%s left)\n",
			silence.ID, silence.Kind, rule, subject, remaining)
	}
	return sb.String()
}

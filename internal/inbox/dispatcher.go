package inbox

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"strings"
	"time"

	"github.com/mmrzaf/raedar/internal/store"
)

// Store is the slice of *store.Store the dispatcher uses.
type Store interface {
	CreateSilence(ctx context.Context, silence store.Silence) (int64, error)
	DeleteSilence(ctx context.Context, id int64) (bool, error)
	AckIncident(ctx context.Context, id int64, at time.Time, by string) (bool, error)
	IncidentByID(ctx context.Context, id int64) (*store.Incident, error)
	OpenIncidents(ctx context.Context) (map[store.IncidentKey]store.Incident, error)
	TelegramMessageDeliveries(ctx context.Context, chatID, messageID int64) ([]store.Delivery, error)
	ActiveSilences(ctx context.Context, now time.Time) ([]store.Silence, error)
}

// notifier is what the dispatcher needs from the pager: a way to answer the
// operator.
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
	// Now returns the current time. Nil means time.Now.
	Now func() time.Time
	// Location formats times in replies. Nil means UTC.
	Location *time.Location
}

// now returns the dispatcher clock.
func (d *Dispatcher) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
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
// returns the reply to send back. The reply text is Telegram HTML, so every
// piece of operator or database text in it is escaped.
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

// SendReply sends the reply to Telegram through the configured notifier. The
// Source loop calls it after Execute returns. It fails when no notifier is
// configured or the channel rejects the send.
func (d *Dispatcher) SendReply(ctx context.Context, result Result) error {
	if result.Reply.Text == "" {
		return nil
	}
	if d.Notifier == nil {
		return fmt.Errorf("no notifier configured for replies")
	}
	return d.Notifier.SendReply(ctx, result.Reply.ChatID, result.Reply.Text)
}

// reply answers with text that is already Telegram HTML.
func (d *Dispatcher) reply(msg Message, text string) Result {
	return Result{Reply: Reply{ChatID: msg.ChatID, Text: text}}
}

// plain answers with text that may contain characters HTML treats as markup,
// such as the <rule> placeholders of a usage message.
func (d *Dispatcher) plain(msg Message, text string) Result {
	return d.reply(msg, html.EscapeString(text))
}

// location returns the zone replies are formatted in.
func (d *Dispatcher) location() *time.Location {
	if d.Location != nil {
		return d.Location
	}
	return time.UTC
}

func (d *Dispatcher) status(ctx context.Context, msg Message) (Result, error) {
	open, err := d.Store.OpenIncidents(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("read open incidents: %w", err)
	}
	now := d.now()
	active, err := d.Store.ActiveSilences(ctx, now)
	if err != nil {
		return Result{}, fmt.Errorf("read silences: %w", err)
	}
	return d.reply(msg, formatStatus(d.Host, open, active, now)), nil
}

func (d *Dispatcher) ack(ctx context.Context, msg Message, cmd Command) (Result, error) {
	id, err := ParseAckArgs(cmd.Args)
	if err != nil {
		return d.plain(msg, err.Error()), nil
	}
	incident, err := d.Store.IncidentByID(ctx, id)
	if err != nil {
		return Result{}, fmt.Errorf("read incident %d: %w", id, err)
	}
	if incident == nil {
		return d.plain(msg, fmt.Sprintf("incident %d does not exist", id)), nil
	}
	if incident.ResolvedAt != nil {
		return d.plain(msg, fmt.Sprintf("incident %d is already resolved", id)), nil
	}
	acked, err := d.Store.AckIncident(ctx, id, d.now(), d.ackBy(msg))
	if err != nil {
		return Result{}, err
	}
	if !acked {
		return d.plain(msg, fmt.Sprintf("incident %d was already acked", id)), nil
	}
	return d.reply(msg, fmt.Sprintf("acked incident %d (%s on %s)",
		id, html.EscapeString(incident.Rule), html.EscapeString(incident.Subject))), nil
}

func (d *Dispatcher) mute(ctx context.Context, msg Message, cmd Command) (Result, error) {
	now := d.now()
	rule, subject, endsAt, err := ParseMuteArgs(cmd.Args, now)
	if err != nil {
		return d.plain(msg, err.Error()), nil
	}
	if rule == "" {
		var problem string
		rule, subject, problem, err = d.repliedTarget(ctx, msg)
		if err != nil {
			return Result{}, err
		}
		if problem != "" {
			return d.plain(msg, problem), nil
		}
	}
	if !endsAt.After(now) {
		return d.plain(msg, "the requested end time is already in the past"), nil
	}
	if endsAt.Sub(now) > store.MaxSilenceDuration {
		return d.plain(msg, fmt.Sprintf("a mute may last at most %d days", int(store.MaxSilenceDuration/(24*time.Hour)))), nil
	}
	id, err := d.Store.CreateSilence(ctx, store.Silence{
		Kind:      store.SilenceOneShotMute,
		Rule:      &rule,
		Subject:   &subject,
		Reason:    d.ackBy(msg),
		StartsAt:  now,
		EndsAt:    endsAt,
		CreatedBy: stringPtr("telegram"),
		Comment:   stringPtr(msg.Text),
	})
	if err != nil {
		return Result{}, fmt.Errorf("record silence: %w", err)
	}
	return d.reply(msg, fmt.Sprintf("muted %s on %s until %s (silence #%d)",
		html.EscapeString(rule), html.EscapeString(subject),
		endsAt.In(d.location()).Format("Mon 02 Jan 15:04 MST"), id)), nil
}

// repliedTarget resolves the (rule, subject) a short `mute` refers to from the
// paged message the operator replied to. When it cannot, problem says why in
// words for the operator.
func (d *Dispatcher) repliedTarget(ctx context.Context, msg Message) (rule, subject, problem string, err error) {
	if msg.ReplyToMessageID == 0 {
		return "", "", "reply to a paged message, or send mute <rule> <subject> <duration>", nil
	}
	deliveries, err := d.Store.TelegramMessageDeliveries(ctx, msg.ChatID, msg.ReplyToMessageID)
	if err != nil {
		return "", "", "", fmt.Errorf("resolve replied-to message: %w", err)
	}
	incidents := make(map[int64]bool)
	for _, delivery := range deliveries {
		if delivery.IncidentID != nil {
			incidents[*delivery.IncidentID] = true
		}
	}
	switch len(incidents) {
	case 0:
		return "", "", "the message you replied to is not a Raedar page I can mute", nil
	case 1:
	default:
		return "", "", fmt.Sprintf("that message covers %d incidents; send mute <rule> <subject> <duration> for the one you want", len(incidents)), nil
	}
	incident, err := d.Store.IncidentByID(ctx, *deliveries[0].IncidentID)
	if err != nil {
		return "", "", "", fmt.Errorf("read incident %d: %w", *deliveries[0].IncidentID, err)
	}
	if incident == nil {
		return "", "", "the incident of that message no longer exists", nil
	}
	return incident.Rule, incident.Subject, "", nil
}

func (d *Dispatcher) unmute(ctx context.Context, msg Message, cmd Command) (Result, error) {
	id, err := ParseUnmuteArgs(cmd.Args)
	if err != nil {
		return d.plain(msg, err.Error()), nil
	}
	deleted, err := d.Store.DeleteSilence(ctx, id)
	if err != nil {
		return Result{}, err
	}
	if !deleted {
		return d.plain(msg, fmt.Sprintf("silence %d does not exist", id)), nil
	}
	return d.plain(msg, fmt.Sprintf("removed silence #%d", id)), nil
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

// helpText is the response to `help`. Replies use Telegram's HTML parse
// mode, so the commands are wrapped in <b>.
func helpText() string {
	return strings.TrimSpace(`
<b>Raedar commands</b>

<b>help</b> — this message
<b>status</b> — open incidents and active silences
<b>ack &lt;id&gt;</b> — acknowledge an incident; all its later messages stop
<b>mute &lt;rule&gt; &lt;subject&gt; &lt;duration&gt;</b> — silence one (rule, subject)
<b>mute &lt;rule&gt; &lt;subject&gt; until &lt;RFC3339&gt;</b> — silence until a timestamp
reply <b>mute 2h</b> — silence the (rule, subject) of a paged message
reply <b>mute until 2026-10-21T08:00:00Z</b> — same, with an explicit end
<b>unmute &lt;silence-id&gt;</b> — remove a silence

Durations are like 30s, 5m, 2h or 14d.
`)
}

// formatStatus renders the status reply as Telegram HTML.
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
		html.EscapeString(host), len(open), critical, warning, len(silences))
	for _, silence := range silences {
		rule := "any"
		if silence.Rule != nil {
			rule = *silence.Rule
		}
		subject := "any"
		if silence.Subject != nil {
			subject = *silence.Subject
		}
		scope := html.EscapeString(rule + " on " + subject)
		if silence.AppID != nil && silence.Rule == nil && silence.Subject == nil {
			scope = fmt.Sprintf("app #%d", *silence.AppID)
		}
		remaining := silence.EndsAt.Sub(now).Round(time.Minute)
		fmt.Fprintf(&sb, "  #%d  %s  %s  (%s left)\n",
			silence.ID, silence.Kind, scope, remaining)
	}
	return sb.String()
}

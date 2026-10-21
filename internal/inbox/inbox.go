// Package inbox polls Telegram for inbound updates and dispatches operator
// commands: ack, mute, unmute, status, help. One goroutine per process, run
// alongside `serve`.
//
// We use long polling on getUpdates instead of a webhook because Raedar is
// behind a CDN-replaced TLS edge, and the operator wants a single binary to
// run without opening a port.
package inbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mmrzaf/raedar/internal/clock"
	"github.com/mmrzaf/raedar/internal/config"
	"github.com/mmrzaf/raedar/internal/notify"
)

// updateEnvelope is one response from getUpdates. Telegram returns
// {"ok":true,"result":[...]}; we treat the failed response as an empty
// payload and log.
type updateEnvelope struct {
	Ok     bool     `json:"ok"`
	Result []update `json:"result"`
}

// Telegram Update types we care about. The webhook types we ignore keep
// their zero value.
type update struct {
	UpdateID int64    `json:"update_id"`
	Message  *message `json:"message,omitempty"`
}

type message struct {
	MessageID int64  `json:"message_id"`
	Text      string `json:"text"`
	Chat      chat   `json:"chat"`
	From      *user  `json:"from,omitempty"`
	// ReplyToMessage is non-nil when the operator replied to a paged
	// message; Raedar uses its message_id to find the original delivery
	// and the (rule, subject) it referred to.
	ReplyToMessage *message `json:"reply_to_message,omitempty"`
}

type chat struct {
	ID int64 `json:"id"`
}

type user struct {
	ID int64 `json:"id"`
}

// Message is one command the inbox parsed out of a Telegram update. The
// caller dispatches it to the operator-commands handler.
type Message struct {
	UpdateID  int64
	MessageID int64
	ChatID    int64
	UserID    int64
	Text      string
	// ReplyToMessageID is the message_id of the message the operator
	// replied to, or 0 when the message was a fresh command.
	ReplyToMessageID int64
	ReceivedAt       time.Time
}

// ClaimFunc claims an update_id. It reports true when this process should
// handle the update and false when another poll (or an earlier run) already
// did. Implementations live in the store package.
type ClaimFunc func(ctx context.Context, updateID int64) (bool, error)

// Source polls Telegram getUpdates and dispatches commands. Each Run
// call blocks until ctx is done.
type Source struct {
	APIURL   string
	Token    string
	ChatIDs  []int64
	LongPoll time.Duration

	// HTTP is the client used for getUpdates. The reply path uses
	// notifier.SendReply, which carries its own client.
	HTTP   *http.Client
	Logger *slog.Logger
	Clock  clock.Clock

	// Claim takes ownership of an update_id.
	Claim ClaimFunc
	// Dispatcher parses and applies every parsed message and writes the
	// reply back to the operator.
	Dispatcher *Dispatcher
}

// pollTimeout bounds one getUpdates request: the long poll plus time for the
// connection and the response.
const pollTimeout = 90 * time.Second

// NewSource returns a Source. claim takes ownership of an update_id and
// dispatcher handles the parsed commands. Polling goes through the outbound
// proxy exactly when Telegram messages do, since a host that cannot reach
// Telegram directly cannot poll it directly either.
func NewSource(cfg *config.Config, logger *slog.Logger, cl clock.Clock, claim ClaimFunc, dispatcher *Dispatcher) (*Source, error) {
	client, err := notify.OutboundClient(cfg, cfg.UsesProxy(config.ProxyTargetTelegram))
	if err != nil {
		return nil, fmt.Errorf("telegram inbox client: %w", err)
	}
	client.Timeout = pollTimeout
	chatIDs := make([]int64, 0, len(cfg.Notify.Telegram.ChatIDs))
	for _, value := range cfg.Notify.Telegram.ChatIDs {
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			logger.Warn("skipping chat id (not numeric)", slog.String("value", value))
			continue
		}
		chatIDs = append(chatIDs, id)
	}
	return &Source{
		APIURL:     cfg.Notify.Telegram.APIURL,
		Token:      cfg.Notify.Telegram.Token,
		ChatIDs:    chatIDs,
		LongPoll:   30 * time.Second,
		HTTP:       client,
		Logger:     logger,
		Clock:      cl,
		Claim:      claim,
		Dispatcher: dispatcher,
	}, nil
}

// Run polls Telegram until ctx is done. Each successfully processed
// update has its update_id recorded as seen.
func (s *Source) Run(ctx context.Context) error {
	if s.Token == "" {
		return nil
	}
	offset := int64(0)
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		msgs, nextOffset, err := s.pull(ctx, offset)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			s.Logger.Warn("telegram getUpdates", slog.String("error", notify.SafeError(err).Error()))
			sleep(ctx, 5*time.Second)
			continue
		}
		// Advance the offset only after the batch is handled; a claim error
		// leaves it alone so the update is retried instead of lost.
		failed := false
		for _, msg := range msgs {
			if !s.chatAllowed(msg.ChatID) {
				continue
			}
			claimed, err := s.Claim(ctx, msg.UpdateID)
			if err != nil {
				s.Logger.Warn("claim update", slog.String("error", err.Error()))
				failed = true
				break
			}
			if !claimed {
				continue
			}
			select {
			case <-ctx.Done():
				return nil
			default:
			}
			result, err := s.Dispatcher.Execute(ctx, msg)
			if err != nil {
				s.Logger.Warn("dispatch command",
					slog.Int64("chat", msg.ChatID),
					slog.String("text", msg.Text),
					slog.String("error", err.Error()))
				result = Result{Reply: Reply{ChatID: msg.ChatID, Text: "could not run that command, see the Raedar log"}}
			}
			if err := s.Dispatcher.SendReply(ctx, result); err != nil {
				s.Logger.Warn("send reply",
					slog.Int64("chat", msg.ChatID),
					slog.String("error", err.Error()))
			}
		}
		if failed {
			sleep(ctx, 5*time.Second)
			continue
		}
		offset = nextOffset
	}
}

// pull performs one long-poll and returns every parsed message plus
// the offset to use on the next call.
func (s *Source) pull(ctx context.Context, offset int64) ([]Message, int64, error) {
	endpoint := fmt.Sprintf("%s/bot%s/getUpdates", s.APIURL, s.Token)
	form := url.Values{
		"offset":          {strconv.FormatInt(offset+1, 10)},
		"timeout":         {strconv.Itoa(int(s.LongPoll.Seconds()))},
		"allowed_updates": {`["message"]`},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, offset, fmt.Errorf("build getUpdates: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := s.HTTP.Do(request)
	if err != nil {
		return nil, offset, fmt.Errorf("getUpdates: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<14))
		return nil, offset, fmt.Errorf("the server answered %s: %s", response.Status, string(body))
	}
	var envelope updateEnvelope
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&envelope); err != nil {
		return nil, offset, fmt.Errorf("decode getUpdates: %w", err)
	}
	if !envelope.Ok {
		return nil, offset, errors.New("getUpdates returned ok=false")
	}
	now := s.Clock.Now()
	out := make([]Message, 0, len(envelope.Result))
	next := offset
	for _, u := range envelope.Result {
		if u.UpdateID > next {
			next = u.UpdateID
		}
		if u.Message == nil || u.Message.Text == "" {
			continue
		}
		msg := Message{
			UpdateID:   u.UpdateID,
			MessageID:  u.Message.MessageID,
			ChatID:     u.Message.Chat.ID,
			Text:       u.Message.Text,
			ReceivedAt: now,
		}
		if u.Message.From != nil {
			msg.UserID = u.Message.From.ID
		}
		if u.Message.ReplyToMessage != nil {
			msg.ReplyToMessageID = u.Message.ReplyToMessage.MessageID
		}
		out = append(out, msg)
	}
	return out, next, nil
}

// chatAllowed reports whether the chat id is one the operator listens to
// in. We deliberately match against an exact list rather than allowing
// every chat — anyone who finds the bot must not be able to drive it.
func (s *Source) chatAllowed(id int64) bool {
	if len(s.ChatIDs) == 0 {
		return false
	}
	for _, allowed := range s.ChatIDs {
		if id == allowed {
			return true
		}
	}
	return false
}

// sleep waits for d, returning early when ctx ends. Sleep cancels cleanly.
func sleep(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

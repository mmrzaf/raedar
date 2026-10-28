package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/mmrzaf/raedar/internal/clock"
	"github.com/mmrzaf/raedar/internal/config"
)

// Kind is a notification channel kind.
type Kind string

// The channel kinds.
const (
	// KindTelegram sends messages through a Telegram bot.
	KindTelegram Kind = "telegram"
	// KindSMS sends messages through the configured SMS provider.
	KindSMS Kind = "sms"
	// KindWebhook posts messages to a URL.
	KindWebhook Kind = "webhook"
)

// Message is one outgoing message.
type Message struct {
	// Text is the message for Telegram.
	Text string
	// SMS is the one-line message for SMS.
	SMS string
	// Document is the webhook JSON body.
	Document []byte
}

// Channel delivers one message.
type Channel interface {
	// Kind returns the channel kind.
	Kind() Kind
	// ChatIDs returns the per-recipient list when the channel fans out
	// (Telegram chats, SMS numbers). Other channels return an empty slice,
	// which the pager treats as "one delivery row per incident".
	ChatIDs() []string
	// Send delivers the message.
	Send(ctx context.Context, message Message) error
}

// newChannels returns the configured channels by kind. proxied and direct
// are the HTTP clients for requests that use the outbound proxy and requests
// that do not.
func newChannels(cfg *config.Config, proxied, direct *http.Client, clock clock.Clock) map[Kind]Channel {
	channels := make(map[Kind]Channel)
	clientFor := func(kind Kind) *http.Client {
		if cfg.UsesProxy(proxyTarget(kind)) {
			return proxied
		}
		return direct
	}
	telegram := cfg.Notify.Telegram
	if telegram.Token != "" && len(telegram.ChatIDs) > 0 {
		channels[KindTelegram] = &telegramChannel{
			apiURL:  telegram.APIURL,
			token:   telegram.Token,
			chatIDs: telegram.ChatIDs,
			client:  clientFor(KindTelegram),
		}
	}
	if cfg.Notify.SMS.HTTP.URL != "" && len(cfg.Notify.SMS.To) > 0 {
		channels[KindSMS] = newSMSChannel(cfg, clientFor(KindSMS))
	}
	if cfg.Notify.Webhook.URL != "" {
		channels[KindWebhook] = &webhookChannel{
			url:    cfg.Notify.Webhook.URL,
			secret: cfg.Notify.Webhook.Secret,
			client: clientFor(KindWebhook),
			clock:  clock,
		}
	}
	return channels
}

// proxyTarget maps a channel kind to its RAEDAR_OUTBOUND_PROXY_FOR name.
func proxyTarget(kind Kind) string {
	switch kind {
	case KindTelegram:
		return config.ProxyTargetTelegram
	case KindSMS:
		return config.ProxyTargetSMS
	case KindWebhook:
		return config.ProxyTargetWebhook
	}
	return ""
}

// chatSend is the outcome of posting one message to one chat. A channel
// that fans out returns one of these per recipient so the pager can mark
// each delivery row independently and retry only the chats that failed.
type chatSend struct {
	chatID    string
	messageID int64
	err       error
}

// fanOut is implemented by channels that post to several recipients and
// can report each result: Telegram and SMS. chats names the recipients for
// this attempt, so a retry can target only the recipients that failed.
type fanOut interface {
	SendEach(ctx context.Context, message Message, chats []string) []chatSend
}

// telegramChannel sends messages through the Telegram Bot API.
type telegramChannel struct {
	apiURL  string
	token   string
	chatIDs []string
	client  *http.Client
}

// Kind returns the channel kind.
func (c *telegramChannel) Kind() Kind { return KindTelegram }

// ChatIDs returns the configured chat_ids for this Telegram channel.
func (c *telegramChannel) ChatIDs() []string { return c.chatIDs }

// SendReply posts a single message to the given chat_id from this Telegram
// channel. The reply goroutine in internal/inbox calls this to acknowledge
// an operator command; it is independent from the notifier's incident
// pager.
func (c *telegramChannel) SendReply(ctx context.Context, chatID, text string) error {
	form := url.Values{
		"chat_id":                  {chatID},
		"text":                     {text},
		"parse_mode":               {"HTML"},
		"disable_web_page_preview": {"true"},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.apiURL+"/bot"+c.token+"/sendMessage", strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("build Telegram reply: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var response sendMessageResponse
	if err := sendJSON(c.client, request, &response); err != nil {
		return fmt.Errorf("send Telegram reply: %w", SafeError(err))
	}
	if !response.Ok {
		return fmt.Errorf("telegram sendMessage returned ok=false")
	}
	return nil
}

// sendMessageResponse is the part of the Telegram sendMessage response we
// read: the assigned message_id, used to look up replies later.
type sendMessageResponse struct {
	Ok     bool `json:"ok"`
	Result struct {
		MessageID int64 `json:"message_id"`
	} `json:"result"`
}

// SendEach posts the message to each requested chat. Every chat is
// attempted even when an earlier one fails, so one dead chat cannot block
// the others and a retry only repeats the chats that actually failed.
func (c *telegramChannel) SendEach(ctx context.Context, message Message, chats []string) []chatSend {
	if len(chats) == 0 {
		chats = c.chatIDs
	}
	results := make([]chatSend, 0, len(chats))
	for _, chatID := range chats {
		form := url.Values{
			"chat_id":                  {chatID},
			"text":                     {message.Text},
			"parse_mode":               {"HTML"},
			"disable_web_page_preview": {"true"},
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost,
			c.apiURL+"/bot"+c.token+"/sendMessage", strings.NewReader(form.Encode()))
		if err != nil {
			results = append(results, chatSend{chatID: chatID, err: fmt.Errorf("build Telegram request: %w", err)})
			continue
		}
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		var response sendMessageResponse
		if err := sendJSON(c.client, request, &response); err != nil {
			results = append(results, chatSend{chatID: chatID, err: telegramRefusal(fmt.Errorf("send Telegram message: %w", SafeError(err)))})
			continue
		}
		if !response.Ok {
			results = append(results, chatSend{chatID: chatID, err: fmt.Errorf("telegram sendMessage returned ok=false")})
			continue
		}
		results = append(results, chatSend{chatID: chatID, messageID: response.Result.MessageID})
	}
	return results
}

// Send posts the message to every chat and returns the first error, if any.
// The notifier uses SendEach directly; Send exists for the generic Channel
// interface used by the notify-test command.
func (c *telegramChannel) Send(ctx context.Context, message Message) error {
	var errs []error
	for _, result := range c.SendEach(ctx, message, nil) {
		if result.err != nil {
			errs = append(errs, result.err)
		}
	}
	return errors.Join(errs...)
}

// newSMSChannel returns the configured SMS provider: a single generic HTTP
// client. Providers that put the API key in the URL path use the {{api_key}}
// placeholder, and providers that take a from line use {{from}}.
func newSMSChannel(cfg *config.Config, client *http.Client) Channel {
	sms := cfg.Notify.SMS
	return &httpSMSChannel{
		url:      sms.HTTP.URL,
		method:   sms.HTTP.Method,
		headers:  sms.HTTP.Headers,
		body:     sms.HTTP.Body,
		encoding: sms.HTTP.Encoding,
		apiKey:   sms.HTTP.APIKey,
		from:     sms.From,
		to:       sms.To,
		client:   client,
	}
}

// httpSMSChannel sends one request per recipient through a generic HTTP API.
// Both URL and Body templates support the placeholders {{to}}, {{message}},
// {{from}} and {{api_key}}; they are substituted in one pass, so a value
// that contains a placeholder is never expanded again.
type httpSMSChannel struct {
	url      string
	method   string
	headers  []config.Header
	body     string
	encoding string
	apiKey   string
	from     string
	to       []string
	client   *http.Client
}

// Kind returns the channel kind.
func (c *httpSMSChannel) Kind() Kind { return KindSMS }

// ChatIDs returns the configured recipient numbers.
func (c *httpSMSChannel) ChatIDs() []string { return slices.Clone(c.to) }

// SendEach makes one request per requested recipient. Every recipient is
// attempted even when an earlier one fails, so a retry only repeats the
// numbers that actually failed.
func (c *httpSMSChannel) SendEach(ctx context.Context, message Message, recipients []string) []chatSend {
	if len(recipients) == 0 {
		recipients = c.to
	}
	results := make([]chatSend, 0, len(recipients))
	for _, recipient := range recipients {
		results = append(results, chatSend{chatID: recipient, err: c.sendOne(ctx, recipient, message.SMS)})
	}
	return results
}

// Send delivers the message to every recipient and returns the joined errors.
func (c *httpSMSChannel) Send(ctx context.Context, message Message) error {
	var errs []error
	for _, result := range c.SendEach(ctx, message, nil) {
		if result.err != nil {
			errs = append(errs, result.err)
		}
	}
	return errors.Join(errs...)
}

// sendOne makes the request for one recipient.
func (c *httpSMSChannel) sendOne(ctx context.Context, recipient, text string) error {
	endpoint := c.expand(c.url, url.QueryEscape, recipient, text)
	body, contentType := c.encode(recipient, text)
	request, err := http.NewRequestWithContext(ctx, c.method, endpoint, strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("build SMS request: %w", err)
	}
	for _, header := range c.headers {
		request.Header.Set(header.Name, header.Value)
	}
	if contentType != "" && request.Header.Get("Content-Type") == "" {
		request.Header.Set("Content-Type", contentType)
	}
	if err := send(c.client, request); err != nil {
		return fmt.Errorf("send SMS: %w", SafeError(err))
	}
	return nil
}

// expand fills a template, passing every value through escape.
func (c *httpSMSChannel) expand(template string, escape func(string) string, recipient, text string) string {
	return strings.NewReplacer(
		"{{to}}", escape(recipient),
		"{{message}}", escape(text),
		"{{from}}", escape(c.from),
		"{{api_key}}", escape(c.apiKey),
	).Replace(template)
}

// encode fills the body template and returns the content type.
func (c *httpSMSChannel) encode(recipient, text string) (string, string) {
	switch c.encoding {
	case "form":
		return c.expand(c.body, url.QueryEscape, recipient, text), "application/x-www-form-urlencoded"
	case "raw":
		return c.expand(c.body, func(value string) string { return value }, recipient, text), ""
	default:
		return c.expand(c.body, escapeJSON, recipient, text), "application/json"
	}
}

// escapeJSON escapes a value for a JSON string, without the quotes.
func escapeJSON(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) < 2 {
		return value
	}
	return string(encoded[1 : len(encoded)-1])
}

// webhookChannel posts messages to a URL.
type webhookChannel struct {
	url    string
	secret string
	client *http.Client
	clock  clock.Clock
}

// Kind returns the channel kind.
func (c *webhookChannel) Kind() Kind { return KindWebhook }

// ChatIDs returns an empty slice; webhooks do not fan out per recipient.
func (c *webhookChannel) ChatIDs() []string { return nil }

// Send posts the JSON body, signed when a secret is configured.
func (c *webhookChannel) Send(ctx context.Context, message Message) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(message.Document))
	if err != nil {
		return fmt.Errorf("build webhook request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if c.secret != "" {
		timestamp := strconv.FormatInt(c.clock.Now().Unix(), 10)
		mac := hmac.New(sha256.New, []byte(c.secret))
		mac.Write([]byte(timestamp + "." + string(message.Document)))
		request.Header.Set("X-Raedar-Timestamp", timestamp)
		request.Header.Set("X-Raedar-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	if err := send(c.client, request); err != nil {
		return fmt.Errorf("send webhook: %w", SafeError(err))
	}
	return nil
}

// send performs one request and accepts a 2xx response.
func send(client *http.Client, request *http.Request) error {
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, io.LimitReader(response.Body, 1<<16)); err != nil {
		return fmt.Errorf("read the response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return &statusError{code: response.StatusCode, msg: fmt.Sprintf("the server answered %s", response.Status)}
	}
	return nil
}

// statusError is a non-2xx answer. Its code tells a refused recipient apart
// from a server that is temporarily unavailable.
type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string { return e.msg }

// refusedError marks a delivery that can never succeed for its recipient,
// such as a chat the bot was removed from. The page is cancelled instead of
// retried, because retrying would repeat the refusal for as long as the
// incident stays open.
type refusedError struct {
	err error
}

func (e *refusedError) Error() string { return e.err.Error() }
func (e *refusedError) Unwrap() error { return e.err }

// telegramRefusal wraps the errors that mean a Telegram chat cannot be
// reached: 403 when the bot was blocked or removed from the chat, and 400
// with "chat not found". Other failures, including 401 for a bad token,
// stay retryable because an operator can fix them without losing the page.
func telegramRefusal(err error) error {
	var status *statusError
	if errors.As(err, &status) && (status.code == http.StatusForbidden || status.code == http.StatusBadRequest) {
		return &refusedError{err: err}
	}
	return err
}

// sendJSON performs one request, accepts a 2xx response and decodes the
// body into out. The body is read with a 64 KiB cap so a misbehaving
// server cannot exhaust memory. An empty body decodes successfully only
// when out is itself a pointer-to-nil kind; most callers pass a typed
// value, so we substitute a permissive empty envelope for the known
// response shapes the package expects.
func sendJSON(client *http.Client, request *http.Request, out any) error {
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<16))
	if err != nil {
		return fmt.Errorf("read the response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return &statusError{code: response.StatusCode, msg: fmt.Sprintf("the server answered %s: %s", response.Status, string(data))}
	}
	// Empty body: substitute permissive envelope so JSON decoding succeeds
	// without misleading the rest of the package.
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		trimmed = []byte(`{"ok":true,"result":{"message_id":0}}`)
	}
	if err := json.Unmarshal(trimmed, out); err != nil {
		return fmt.Errorf("decode the response: %w", err)
	}
	return nil
}

// SafeError removes the request URL from an error, because URLs can contain
// secrets such as a Telegram bot token. Log errors from outbound requests
// through it.
func SafeError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("%s: %w", urlErr.Op, urlErr.Err)
	}
	return err
}

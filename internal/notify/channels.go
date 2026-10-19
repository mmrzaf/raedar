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
	"strconv"
	"strings"

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
	// Send delivers the message.
	Send(ctx context.Context, message Message) error
}

// newChannels returns the configured channels by kind. proxied and direct
// are the HTTP clients for requests that use the outbound proxy and requests
// that do not.
func newChannels(cfg *config.Config, proxied, direct *http.Client, clock Clock) map[Kind]Channel {
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
	if cfg.Notify.SMS.Provider != "" && len(cfg.Notify.SMS.To) > 0 {
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

// telegramChannel sends messages through the Telegram Bot API.
type telegramChannel struct {
	apiURL  string
	token   string
	chatIDs []string
	client  *http.Client
}

// Kind returns the channel kind.
func (c *telegramChannel) Kind() Kind { return KindTelegram }

// Send posts the message to every chat with HTML formatting and link previews
// disabled.
func (c *telegramChannel) Send(ctx context.Context, message Message) error {
	for _, chatID := range c.chatIDs {
		form := url.Values{
			"chat_id":                  {chatID},
			"text":                     {message.Text},
			"parse_mode":               {"HTML"},
			"disable_web_page_preview": {"true"},
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost,
			c.apiURL+"/bot"+c.token+"/sendMessage", strings.NewReader(form.Encode()))
		if err != nil {
			return fmt.Errorf("build Telegram request: %w", err)
		}
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if err := send(c.client, request); err != nil {
			return fmt.Errorf("send Telegram message: %w", safeError(err))
		}
	}
	return nil
}

// newSMSChannel returns the configured SMS provider.
func newSMSChannel(cfg *config.Config, client *http.Client) Channel {
	sms := cfg.Notify.SMS
	if sms.Provider == "http" {
		return &httpSMSChannel{
			url:      sms.HTTP.URL,
			method:   sms.HTTP.Method,
			headers:  sms.HTTP.Headers,
			body:     sms.HTTP.Body,
			encoding: sms.HTTP.Encoding,
			to:       sms.To,
			client:   client,
		}
	}
	return &kavenegarChannel{
		apiKey:  sms.KavenegarAPIKey,
		sender:  sms.KavenegarSender,
		to:      sms.To,
		client:  client,
		baseURL: "https://api.kavenegar.com",
	}
}

// kavenegarChannel sends messages through Kavenegar.
type kavenegarChannel struct {
	apiKey  string
	sender  string
	to      []string
	client  *http.Client
	baseURL string
}

// Kind returns the channel kind.
func (c *kavenegarChannel) Kind() Kind { return KindSMS }

// Send makes one request for all recipients. The API key is part of the URL,
// so errors never contain it.
func (c *kavenegarChannel) Send(ctx context.Context, message Message) error {
	form := url.Values{
		"receptor": {strings.Join(c.to, ",")},
		"message":  {message.SMS},
	}
	if c.sender != "" {
		form.Set("sender", c.sender)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v1/"+c.apiKey+"/sms/send.json", strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("build Kavenegar request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := c.client.Do(request)
	if err != nil {
		return fmt.Errorf("send Kavenegar message: %w", safeError(err))
	}
	defer response.Body.Close()
	var body struct {
		Return struct {
			Status  int    `json:"status"`
			Message string `json:"message"`
		} `json:"return"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<16)).Decode(&body); err != nil {
		return fmt.Errorf("send Kavenegar message: read the response: %w", err)
	}
	if body.Return.Status != 200 {
		return fmt.Errorf("send Kavenegar message: status %d: %s", body.Return.Status, body.Return.Message)
	}
	return nil
}

// httpSMSChannel sends one request per recipient through a generic HTTP API.
type httpSMSChannel struct {
	url      string
	method   string
	headers  []config.Header
	body     string
	encoding string
	to       []string
	client   *http.Client
}

// Kind returns the channel kind.
func (c *httpSMSChannel) Kind() Kind { return KindSMS }

// Send makes one request per recipient.
func (c *httpSMSChannel) Send(ctx context.Context, message Message) error {
	for _, recipient := range c.to {
		endpoint := strings.ReplaceAll(c.url, "{{to}}", url.QueryEscape(recipient))
		endpoint = strings.ReplaceAll(endpoint, "{{message}}", url.QueryEscape(message.SMS))
		body, contentType := c.encode(recipient, message.SMS)
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
			return fmt.Errorf("send SMS: %w", safeError(err))
		}
	}
	return nil
}

// encode fills the body template and returns the content type.
func (c *httpSMSChannel) encode(recipient, message string) (string, string) {
	switch c.encoding {
	case "form":
		body := strings.ReplaceAll(c.body, "{{to}}", url.QueryEscape(recipient))
		body = strings.ReplaceAll(body, "{{message}}", url.QueryEscape(message))
		return body, "application/x-www-form-urlencoded"
	case "raw":
		body := strings.ReplaceAll(c.body, "{{to}}", recipient)
		body = strings.ReplaceAll(body, "{{message}}", message)
		return body, ""
	default:
		body := strings.ReplaceAll(c.body, "{{to}}", escapeJSON(recipient))
		body = strings.ReplaceAll(body, "{{message}}", escapeJSON(message))
		return body, "application/json"
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
	clock  Clock
}

// Kind returns the channel kind.
func (c *webhookChannel) Kind() Kind { return KindWebhook }

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
		return fmt.Errorf("send webhook: %w", safeError(err))
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
		return fmt.Errorf("the server answered %s", response.Status)
	}
	return nil
}

// safeError removes the request URL from an error, because URLs can contain
// secrets.
func safeError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("%s: %w", urlErr.Op, urlErr.Err)
	}
	return err
}

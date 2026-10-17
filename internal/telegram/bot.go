// Package telegram is a long-polling bot in the burnrate process.
// Only the configured chat id receives messages.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const defaultSendTimeout = 15 * time.Second

var httpURL = regexp.MustCompile(`https?://[^\s"]+`)

// Handler answers bot commands from local data.
type Handler interface {
	Status(ctx context.Context) (string, error)
	Forecast(ctx context.Context) (string, error)
}

// Client talks to the Telegram Bot API.
// SendTimeout bounds sendMessage. Zero uses defaultSendTimeout.
// GetUpdates uses the long-poll timeout instead, so a slow poll is not cut
// short by the send deadline.
type Client struct {
	Token       string
	BaseURL     string
	HTTP        *http.Client
	SendTimeout time.Duration
}

// Update is one getUpdates result.
type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message"`
}

// Message is a private or group message.
type Message struct {
	Text string `json:"text"`
	Chat Chat   `json:"chat"`
}

// Chat identifies the conversation.
type Chat struct {
	ID int64 `json:"id"`
}

func (c *Client) sendTimeout() time.Duration {
	if c != nil && c.SendTimeout > 0 {
		return c.SendTimeout
	}
	return defaultSendTimeout
}

func (c *Client) endpoint(method string) string {
	base := "https://api.telegram.org"
	if c.BaseURL != "" {
		base = strings.TrimRight(c.BaseURL, "/")
	}
	return base + "/bot" + c.Token + "/" + method
}

// redact strips the bot token and any URL. Transport errors from net/http
// include the request URL, and that URL contains the token.
func (c *Client) redact(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	var uerr *url.Error
	if errors.As(err, &uerr) && uerr.Err != nil {
		// url.Error prints the full request URL, which contains the bot token.
		msg = uerr.Op + ": " + uerr.Err.Error()
		if parsed, perr := url.Parse(uerr.URL); perr == nil && parsed.Host != "" {
			msg = strings.ReplaceAll(msg, parsed.Host, "[redacted-url]")
		}
	}
	if c != nil && c.Token != "" {
		msg = strings.ReplaceAll(msg, c.Token, "[redacted]")
	}
	msg = httpURL.ReplaceAllString(msg, "[redacted-url]")
	if msg == err.Error() {
		return err
	}
	return errors.New(msg)
}

func (c *Client) do(ctx context.Context, client *http.Client, method string, body any, out any) error {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		return c.redact(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(method), &buf)
	if err != nil {
		return c.redact(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return c.redact(err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return c.redact(err)
	}
	if resp.StatusCode != http.StatusOK {
		return c.redact(fmt.Errorf("telegram %s: http %d", method, resp.StatusCode))
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return c.redact(err)
	}
	return nil
}

// Send delivers text to one chat. The token is never included in errors.
func (c *Client) Send(ctx context.Context, chatID int64, text string) error {
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("empty telegram message")
	}
	if len(text) > 4000 {
		text = text[:4000]
	}
	var resp struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	timeout := c.sendTimeout()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	err := c.do(ctx, client, "sendMessage", map[string]any{
		"chat_id":                  chatID,
		"text":                     text,
		"disable_web_page_preview": true,
	}, &resp)
	if err != nil {
		return err
	}
	if !resp.OK {
		if resp.Description == "" {
			resp.Description = "not ok"
		}
		return c.redact(fmt.Errorf("telegram sendMessage: %s", resp.Description))
	}
	return nil
}

// GetUpdates long-polls. timeout is the server-side wait in seconds.
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout int) ([]Update, error) {
	var resp struct {
		OK          bool     `json:"ok"`
		Description string   `json:"description"`
		Result      []Update `json:"result"`
	}
	if timeout < 0 {
		timeout = 0
	}
	// The Bot API holds the request open for timeout seconds. The send
	// deadline must not apply here.
	wait := time.Duration(timeout)*time.Second + 10*time.Second
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: wait}
	}
	err := c.do(ctx, client, "getUpdates", map[string]any{
		"offset":  offset,
		"timeout": timeout,
	}, &resp)
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		if resp.Description == "" {
			resp.Description = "not ok"
		}
		return nil, c.redact(fmt.Errorf("telegram getUpdates: %s", resp.Description))
	}
	return resp.Result, nil
}

// Bot serves /status and /forecast for a single chat.
type Bot struct {
	Client  *Client
	ChatID  int64
	Handler Handler
}

const helpText = "burnrate\n/status — current subscription windows\n/forecast — pace and when the limit will be hit"

// Command returns the slash command without @BotName.
func Command(text string) (string, bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return "", false
	}
	field := text
	if i := strings.IndexAny(text, " \t\n"); i >= 0 {
		field = text[:i]
	}
	field = strings.TrimPrefix(field, "/")
	if i := strings.IndexByte(field, '@'); i >= 0 {
		field = field[:i]
	}
	field = strings.ToLower(field)
	if field == "" {
		return "", false
	}
	return field, true
}

// Handle replies to one update. Messages from other chats are ignored.
func (b *Bot) Handle(ctx context.Context, u Update) error {
	if b == nil || b.Client == nil || b.Handler == nil || u.Message == nil {
		return nil
	}
	if u.Message.Chat.ID != b.ChatID {
		return nil
	}
	cmd, ok := Command(u.Message.Text)
	if !ok {
		return nil
	}
	var text string
	var err error
	switch cmd {
	case "status":
		text, err = b.Handler.Status(ctx)
	case "forecast":
		text, err = b.Handler.Forecast(ctx)
	case "start", "help":
		text = helpText
	default:
		text = helpText
	}
	if err != nil {
		text = "burnrate could not read local data."
	}
	return b.Client.Send(ctx, b.ChatID, text)
}

// Run long-polls until ctx is cancelled.
func (b *Bot) Run(ctx context.Context) error {
	var offset int64
	for {
		if ctx.Err() != nil {
			return nil
		}
		updates, err := b.Client.GetUpdates(ctx, offset, 25)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(2 * time.Second):
			}
			continue
		}
		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			if err := b.Handle(ctx, u); err != nil && ctx.Err() == nil {
				// Keep polling. The next command can try again.
				continue
			}
		}
	}
}

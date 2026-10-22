package telegram

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCommand(t *testing.T) {
	name, ok := Command("/status@burnrate_bot now")
	if !ok || name != "status" {
		t.Fatalf("got %q %v", name, ok)
	}
	if _, ok := Command("hello"); ok {
		t.Fatal("plain text is not a command")
	}
	name, ok = Command("/Forecast")
	if !ok || name != "forecast" {
		t.Fatalf("got %q %v", name, ok)
	}
}

type fakeHandler struct {
	status string
	err    error
}

func (f fakeHandler) Status(context.Context) (string, error) { return f.status, f.err }
func (f fakeHandler) Forecast(context.Context) (string, error) {
	return "forecast body", nil
}

func TestHandleAllowedChatOnly(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var msg struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(body, &msg)
		mu.Lock()
		sent = append(sent, msg.Text)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{}})
	}))
	defer srv.Close()

	bot := &Bot{
		Client:  &Client{Token: "secret-token", BaseURL: srv.URL},
		ChatID:  42,
		Handler: fakeHandler{status: "Claude 5h 10%"},
	}
	ctx := context.Background()
	if err := bot.Handle(ctx, Update{Message: &Message{Text: "/status", Chat: Chat{ID: 99}}}); err != nil {
		t.Fatal(err)
	}
	if err := bot.Handle(ctx, Update{Message: &Message{Text: "hello", Chat: Chat{ID: 42}}}); err != nil {
		t.Fatal(err)
	}
	if err := bot.Handle(ctx, Update{Message: &Message{Text: "/status", Chat: Chat{ID: 42}}}); err != nil {
		t.Fatal(err)
	}
	if err := bot.Handle(ctx, Update{Message: &Message{Text: "/forecast", Chat: Chat{ID: 42}}}); err != nil {
		t.Fatal(err)
	}
	bot.Handler = fakeHandler{err: io.EOF}
	if err := bot.Handle(ctx, Update{Message: &Message{Text: "/status", Chat: Chat{ID: 42}}}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 3 {
		t.Fatalf("sent = %#v", sent)
	}
	if sent[0] != "Claude 5h 10%" || sent[1] != "forecast body" {
		t.Fatalf("sent = %#v", sent)
	}
	if sent[2] != "burnrate could not read local data." {
		t.Fatalf("error reply = %q", sent[2])
	}
	for _, text := range sent {
		if strings.Contains(text, "secret-token") {
			t.Fatal("reply contains the bot token")
		}
	}
}

func TestSendRedactsTokenAndURL(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	token := "123456:SUPERSECRET"
	c := &Client{Token: token, BaseURL: "http://" + addr, SendTimeout: 50 * time.Millisecond}
	err = c.Send(context.Background(), 1, "hi")
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if strings.Contains(msg, token) || strings.Contains(msg, "http://") || strings.Contains(msg, addr) {
		t.Fatalf("transport error leaked token or url: %s", msg)
	}
}

func TestSendTimeoutDoesNotCapLongPoll(t *testing.T) {
	token := "123456:SUPERSECRET"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "sendMessage") {
			time.Sleep(300 * time.Millisecond)
			w.WriteHeader(http.StatusGatewayTimeout)
			return
		}
		time.Sleep(200 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": []any{}})
	}))
	defer srv.Close()
	c := &Client{Token: token, BaseURL: srv.URL, SendTimeout: 50 * time.Millisecond}
	start := time.Now()
	err := c.Send(context.Background(), 1, "hi")
	if time.Since(start) > time.Second {
		t.Fatalf("send took %s", time.Since(start))
	}
	if err == nil || strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "http") {
		t.Fatalf("err = %v", err)
	}
	start = time.Now()
	ups, err := c.GetUpdates(context.Background(), 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 0 {
		t.Fatalf("updates = %#v", ups)
	}
	if time.Since(start) < 150*time.Millisecond {
		t.Fatalf("getUpdates returned too fast: %s", time.Since(start))
	}
}

func TestGetUpdatesBackoff(t *testing.T) {
	token := "123456:SUPERSECRET"
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()
	var logs strings.Builder
	bot := &Bot{
		Client: &Client{Token: token, BaseURL: srv.URL},
		ChatID: 1,
		Log:    log.New(&logs, "", 0),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3200*time.Millisecond)
	defer cancel()
	if err := bot.Run(ctx); err != nil {
		t.Fatal(err)
	}
	n := hits.Load()
	if n < 2 || n > 5 {
		t.Fatalf("requests = %d, log = %s", n, logs.String())
	}
	text := logs.String()
	if strings.Contains(text, token) || strings.Contains(text, "http://") {
		t.Fatalf("log leaked token or url: %s", text)
	}
	if !strings.Contains(text, "401") {
		t.Fatalf("log = %s", text)
	}
	if strings.Count(text, "getUpdates") > 2 {
		t.Fatalf("logs not rate limited: %s", text)
	}
}

func TestLogLimitedIsPerKind(t *testing.T) {
	var logs strings.Builder
	b := &Bot{Log: log.New(&logs, "", 0)}
	b.logLimited("getUpdates", "telegram getUpdates: http 401")
	b.logLimited("getUpdates", "telegram getUpdates: http 401")
	b.logLimited("handle", "telegram handle: send failed")
	text := logs.String()
	if strings.Count(text, "getUpdates") != 1 || strings.Count(text, "handle") != 1 {
		t.Fatalf("log = %s", text)
	}
}

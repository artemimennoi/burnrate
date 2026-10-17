package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
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

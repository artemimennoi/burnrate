package agent

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/artemimennoi/burnrate/internal/collect/codex"
	"github.com/artemimennoi/burnrate/internal/provider"
	"github.com/artemimennoi/burnrate/internal/store"
	"github.com/artemimennoi/burnrate/internal/telegram"
)

func TestTickAlertsAndDedup(t *testing.T) {
	root := filepath.Join(t.TempDir(), "SECRET_PATH_DO_NOT_STORE", "2026", "10", "17")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	fiveReset := time.Date(2026, 10, 17, 16, 0, 0, 0, time.UTC).Unix()
	weekReset := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC).Unix()
	line := `{"timestamp":"2026-10-17T12:00:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":"codex","plan_type":"plus","primary":{"used_percent":92,"window_minutes":300,"resets_at":` + itoa(fiveReset) + `},"secondary":{"used_percent":10,"window_minutes":10080,"resets_at":` + itoa(weekReset) + `}}}}` + "\n"
	line += `{"timestamp":"2026-10-17T12:01:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":"premium","primary":null,"secondary":null}}}` + "\n"
	if err := os.WriteFile(filepath.Join(root, "rollout-demo.jsonl"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var msg struct {
			ChatID int64  `json:"chat_id"`
			Text   string `json:"text"`
		}
		_ = json.Unmarshal(body, &msg)
		if msg.ChatID != 42 {
			t.Errorf("chat = %d", msg.ChatID)
		}
		mu.Lock()
		sent = append(sent, msg.Text)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "burnrate.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var logs strings.Builder
	ag := &Agent{
		Collectors: []provider.Collector{codex.New(filepath.Dir(filepath.Dir(filepath.Dir(root))))},
		Store:      st,
		Telegram:   &telegram.Client{Token: "test-token", BaseURL: srv.URL, HTTP: srv.Client()},
		ChatID:     42,
		Thresholds: []int{50, 75, 90},
		Location:   time.UTC,
		Now:        func() time.Time { return time.Date(2026, 10, 17, 12, 30, 0, 0, time.UTC) },
		Log:        log.New(&logs, "", 0),
	}
	if err := ag.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := append([]string(nil), sent...)
	mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("alerts = %#v\nlog=%s", got, logs.String())
	}
	if !strings.Contains(got[0], "alert 90%") || !strings.Contains(got[0], "Codex (plus) 5h is at 92.0%") {
		t.Fatalf("alert = %s", got[0])
	}
	if strings.Contains(got[0], "alert 50%") || strings.Contains(got[0], "alert 75%") {
		t.Fatalf("lower thresholds were sent: %s", got[0])
	}
	if strings.Contains(got[0], "SECRET_PATH_DO_NOT_STORE") || strings.Contains(got[0], "premium") {
		t.Fatalf("alert leaked data: %s", got[0])
	}
	marked, err := st.SentThresholds(context.Background(), "codex", "codex", "5h", time.Date(2026, 10, 17, 16, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if !marked[50] || !marked[75] || !marked[90] {
		t.Fatalf("marked = %#v", marked)
	}
	status, err := ag.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, "92.0%") || strings.Contains(status, "SECRET_PATH") {
		t.Fatalf("status = %s", status)
	}
	forecast, err := ag.Forecast(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(forecast, "Pace") {
		t.Fatalf("forecast = %s", forecast)
	}

	if err := ag.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 1 {
		t.Fatalf("dedup failed, sent %d: %#v", len(sent), sent)
	}
}

func TestDisabledProviderIsLogged(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "burnrate.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var logs strings.Builder
	ag := &Agent{
		Collectors: []provider.Collector{provider.Disabled{ID: "cursor"}},
		Store:      st,
		Thresholds: []int{50, 75, 90},
		Location:   time.UTC,
		Log:        log.New(&logs, "", 0),
	}
	if err := ag.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "not implemented") {
		t.Fatalf("log = %s", logs.String())
	}
}

func TestAlertLogRedactsTelegramToken(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	token := "999:TELEGRAMTOKENSECRET"
	st, err := store.Open(filepath.Join(t.TempDir(), "burnrate.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	resets := time.Date(2026, 10, 17, 18, 0, 0, 0, time.UTC)
	if err := st.SaveSnapshot(context.Background(), provider.Snapshot{
		Provider: "codex", ObservedAt: resets.Add(-time.Hour), LimitID: "codex", PlanType: "plus",
		Origin: "origin", SourceKey: "src",
		Windows: []provider.Window{{
			Key: "5h", Slot: "primary", UsedPercent: 92, WindowMinutes: 300, ResetsAt: resets,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	ag := &Agent{
		Store: st,
		Telegram: &telegram.Client{
			Token:       token,
			BaseURL:     "http://" + addr,
			SendTimeout: 200 * time.Millisecond,
		},
		ChatID:     7,
		Thresholds: []int{50, 75, 90},
		Location:   time.UTC,
		Now:        func() time.Time { return resets.Add(-time.Hour) },
		Log:        log.New(&logs, "", 0),
	}
	if err := ag.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	text := logs.String()
	if strings.Contains(text, token) || strings.Contains(text, addr) || strings.Contains(text, "http://") {
		t.Fatalf("log leaked token or url: %s", text)
	}
	if !strings.Contains(text, "telegram send failed") {
		t.Fatalf("log = %s", text)
	}
}

func TestTickSendStaysInsideInterval(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(3 * time.Second)
		io.Copy(io.Discard, r.Body)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "burnrate.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	resets := time.Date(2026, 10, 17, 18, 0, 0, 0, time.UTC)
	if err := st.SaveSnapshot(context.Background(), provider.Snapshot{
		Provider: "codex", ObservedAt: resets.Add(-time.Hour), LimitID: "codex", PlanType: "plus",
		Origin: "origin",
		Windows: []provider.Window{{
			Key: "5h", Slot: "primary", UsedPercent: 92, WindowMinutes: 300, ResetsAt: resets,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	ag := &Agent{
		Store: st,
		Telegram: &telegram.Client{
			Token:       "blocked-token",
			BaseURL:     srv.URL,
			SendTimeout: 5 * time.Second,
		},
		ChatID:     7,
		Thresholds: []int{50, 75, 90},
		Location:   time.UTC,
		Interval:   200 * time.Millisecond,
		Now:        func() time.Time { return resets.Add(-time.Hour) },
		Log:        log.New(io.Discard, "", 0),
	}
	start := time.Now()
	if err := ag.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if hits.Load() != 1 {
		t.Fatalf("hits = %d", hits.Load())
	}
	if elapsed > time.Second || elapsed < 50*time.Millisecond {
		t.Fatalf("tick took %s", elapsed)
	}
}

func TestTickBackoffSkipsTelegram(t *testing.T) {
	token := "401:TELEGRAMTOKENSECRET"
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "burnrate.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	resets := time.Date(2026, 10, 17, 18, 0, 0, 0, time.UTC)
	if err := st.SaveSnapshot(context.Background(), provider.Snapshot{
		Provider: "codex", ObservedAt: resets.Add(-time.Hour), LimitID: "codex", PlanType: "plus",
		Origin: "origin",
		Windows: []provider.Window{{
			Key: "5h", Slot: "primary", UsedPercent: 92, WindowMinutes: 300, ResetsAt: resets,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	ag := &Agent{
		Store:      st,
		Telegram:   &telegram.Client{Token: token, BaseURL: srv.URL, SendTimeout: time.Second},
		ChatID:     7,
		Thresholds: []int{50, 75, 90},
		Location:   time.UTC,
		Interval:   time.Second,
		Now:        func() time.Time { return resets.Add(-time.Hour) },
		Log:        log.New(&logs, "", 0),
	}
	if err := ag.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := ag.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, log = %s", hits.Load(), logs.String())
	}
	if strings.Contains(logs.String(), token) {
		t.Fatalf("log leaked token: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "telegram send failed") {
		t.Fatalf("log = %s", logs.String())
	}
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}

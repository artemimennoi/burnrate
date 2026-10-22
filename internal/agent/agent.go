// Package agent runs collectors, threshold alerts, and the Telegram bot.
package agent

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/artemimennoi/burnrate/internal/alert"
	"github.com/artemimennoi/burnrate/internal/analysis"
	"github.com/artemimennoi/burnrate/internal/collect/claude"
	"github.com/artemimennoi/burnrate/internal/collect/codex"
	"github.com/artemimennoi/burnrate/internal/config"
	"github.com/artemimennoi/burnrate/internal/provider"
	"github.com/artemimennoi/burnrate/internal/store"
	"github.com/artemimennoi/burnrate/internal/telegram"
)

// Agent is one pass of collection plus alerting, or a long-running loop.
type Agent struct {
	Collectors []provider.Collector
	Store      *store.Store
	Telegram   *telegram.Client
	ChatID     int64
	Thresholds []int
	Location   *time.Location
	Interval   time.Duration
	Now        func() time.Time
	Log        *log.Logger
	logMu      sync.Mutex
	lastLog    map[string]time.Time
	lastPrune  time.Time
}

// pruneEvery is how often the agent deletes expired rows. status and forecast
// do not prune.
const pruneEvery = time.Hour

// Collectors builds the enabled collectors. Cursor and Grok stay disabled
// unless the config opts in, and even then they do not perform network I/O.
// lg receives per-file read errors so one bad file does not stop the scan.
func Collectors(cfg config.Config, lg *log.Logger) ([]provider.Collector, error) {
	var out []provider.Collector
	if cfg.Codex.Enabled {
		dir, err := cfg.ResolvedSessionsDir()
		if err != nil {
			return nil, err
		}
		col := codex.New(dir)
		col.Log = lg
		out = append(out, col)
	}
	if cfg.Claude.Enabled {
		dir, err := cfg.ResolvedProjectsDir()
		if err != nil {
			return nil, err
		}
		col := claude.New(dir)
		col.Log = lg
		out = append(out, col)
	}
	if cfg.Cursor.Enabled {
		out = append(out, provider.Disabled{ID: "cursor"})
	}
	if cfg.Grok.Enabled {
		out = append(out, provider.Disabled{ID: "grok"})
	}
	return out, nil
}

func (a *Agent) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Agent) logf(format string, args ...any) {
	if a.Log != nil {
		a.Log.Printf(format, args...)
	}
}

func (a *Agent) logLimited(kind, format string, args ...any) {
	if a == nil || a.Log == nil || kind == "" {
		return
	}
	a.logMu.Lock()
	defer a.logMu.Unlock()
	if a.lastLog == nil {
		a.lastLog = map[string]time.Time{}
	}
	if last := a.lastLog[kind]; !last.IsZero() && time.Since(last) < 30*time.Second {
		return
	}
	a.lastLog[kind] = time.Now()
	a.Log.Printf(format, args...)
}

func (a *Agent) views(ctx context.Context) ([]analysis.View, error) {
	series, err := a.Store.LatestSeries(ctx)
	if err != nil {
		return nil, err
	}
	now := a.now()
	views := make([]analysis.View, 0, len(series))
	for _, s := range series {
		views = append(views, analysis.Evaluate(s, now))
	}
	return views, nil
}

// Status implements telegram.Handler.
func (a *Agent) Status(ctx context.Context) (string, error) {
	views, err := a.views(ctx)
	if err != nil {
		return "", err
	}
	return analysis.FormatStatus(views, a.Location), nil
}

// Forecast implements telegram.Handler.
func (a *Agent) Forecast(ctx context.Context) (string, error) {
	views, err := a.views(ctx)
	if err != nil {
		return "", err
	}
	return analysis.FormatForecast(views, a.Location), nil
}

// Scan runs every collector once.
func (a *Agent) Scan(ctx context.Context) {
	for _, c := range a.Collectors {
		stats, err := c.Collect(ctx, a.Store)
		if err != nil {
			a.logf("%s: %v", c.Name(), err)
			continue
		}
		a.logf("%s: files=%d snapshots=%d tokens=%d skipped=%d malformed=%d",
			c.Name(), stats.Files, stats.Snapshots, stats.Tokens, stats.Skipped, stats.Malformed)
	}
}

// Tick scans local sources and sends any new threshold alerts.
// Expired rows are deleted at most once an hour.
func (a *Agent) Tick(ctx context.Context) error {
	a.maybePrune(ctx)
	a.Scan(ctx)
	return a.alert(ctx)
}

func (a *Agent) maybePrune(ctx context.Context) {
	if a == nil || a.Store == nil {
		return
	}
	now := a.now()
	if !a.lastPrune.IsZero() && now.Sub(a.lastPrune) < pruneEvery {
		return
	}
	if err := a.Store.Prune(ctx); err != nil {
		a.logf("prune: %v", err)
		return
	}
	a.lastPrune = now
}

func (a *Agent) alert(ctx context.Context) error {
	views, err := a.views(ctx)
	if err != nil {
		return err
	}
	budget := a.Interval
	if budget <= 0 {
		budget = 30 * time.Second
	}
	sendCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	waiting := 0
	type job struct {
		view    analysis.View
		pending []int
	}
	var jobs []job
	for _, v := range views {
		sent, err := a.Store.SentThresholds(ctx, v.Series.Provider, v.Series.LimitID, v.Series.WindowKey, v.Series.ResetsAt)
		if err != nil {
			return err
		}
		pending := alert.Pending(v.Used, a.Thresholds, sent, v.Forecast.Kind == analysis.KindStale)
		if len(pending) == 0 {
			continue
		}
		if a.Telegram == nil || a.ChatID == 0 {
			waiting += len(pending)
			continue
		}
		if !a.Telegram.AllowSend() {
			a.logLimited("send", "telegram send skipped during backoff")
			continue
		}
		jobs = append(jobs, job{view: v, pending: pending})
	}
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			a.deliver(sendCtx, j.view, j.pending)
		}(j)
	}
	wg.Wait()
	if waiting > 0 {
		a.logf("telegram not configured; %d alerts pending", waiting)
	}
	return nil
}

func (a *Agent) deliver(ctx context.Context, v analysis.View, pending []int) {
	th, ok := alert.Highest(pending)
	if !ok {
		return
	}
	text := analysis.FormatAlert(v, th, a.Location)
	if err := a.Telegram.Send(ctx, a.ChatID, text); err != nil {
		a.Telegram.NoteFailure()
		a.logLimited("send", "telegram send failed: %v", err)
		return
	}
	a.Telegram.NoteSuccess()
	for _, mark := range pending {
		if err := a.Store.MarkAlert(ctx, v.Series.Provider, v.Series.LimitID, v.Series.WindowKey, v.Series.ResetsAt, mark, v.Used, a.now()); err != nil {
			a.logf("record alert: %v", err)
		}
	}
}

// Run scans immediately, then on each interval, and long-polls Telegram until ctx ends.
func (a *Agent) Run(ctx context.Context) error {
	a.logf("agent started")
	if a.Telegram != nil && a.ChatID != 0 {
		a.logf("telegram enabled")
		go func() {
			bot := &telegram.Bot{Client: a.Telegram, ChatID: a.ChatID, Handler: a, Log: a.Log}
			if err := bot.Run(ctx); err != nil && ctx.Err() == nil {
				a.logLimited("bot", "telegram bot: %v", err)
			}
		}()
	} else {
		a.logf("telegram disabled")
	}
	if err := a.Tick(ctx); err != nil && ctx.Err() == nil {
		a.logf("tick: %v", err)
	}
	interval := a.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := a.Tick(ctx); err != nil && ctx.Err() == nil {
				a.logf("tick: %v", err)
			}
		}
	}
}

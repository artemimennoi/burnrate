# burnrate

burnrate is a local tracker for how fast **AI subscription** limits get used up. It watches the rolling windows on a Claude and Codex plan, forecasts when the current pace will hit the limit, and sends a Telegram alert at 50%, 75%, and 90%. It does not meter API keys.

One static Go binary, for macOS and Linux. SQLite is bundled with a pure-Go driver, so the build does not need CGO.

## What it records

- **Claude Code** subscription windows, from the [status line JSON](https://code.claude.com/docs/en/statusline) (Claude Code v2.1.80+). The payload includes `rate_limits.five_hour` and `rate_limits.seven_day`, each with `used_percentage` and `resets_at` (Unix seconds). A gateway `spend_limit` window is recorded too when it is present. Token counts come from `usage` on assistant rows in `~/.claude/projects/**/*.jsonl` (input, output, cache creation, cache read). Streaming rows that share a message id keep the largest `output_tokens`.
- **Codex** windows, from `~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl`. A `token_count` event carries `rate_limits.primary` and `rate_limits.secondary` (`used_percent`, `window_minutes`, `resets_at`). The window is labeled by its length, so a weekly window in the primary slot still reads as 7d. See [Codex rate limits, read from the rollout file](https://agenttop.dev/blog/codex-rate-limits/). Snapshots with null windows (`limit_id` `premium`) are skipped and are never stored as 0%.

Cursor and Grok are phase 2. The provider interface is in place; those collectors are off and do no network I/O.

burnrate does **not** read OAuth tokens, API keys, or cookies, and it does **not** call `api/oauth/usage`.

## Install

```bash
git clone https://github.com/artemimennoi/burnrate.git
cd burnrate
make build          # bin/burnrate, CGO disabled
mkdir -p "$HOME/bin"
install -m 755 bin/burnrate "$HOME/bin/burnrate"
```

Copy the example config and edit it:

```bash
mkdir -p ~/.config/burnrate
cp config.example.toml ~/.config/burnrate/config.toml
```

The database defaults to `~/.local/share/burnrate/burnrate.db` (`$XDG_DATA_HOME` when that is set). Reset times use the timezone in the config. An empty timezone uses the system zone. The example config sets `Europe/Minsk`.

Install the background agent (launchd on macOS, a systemd user unit on Linux):

```bash
burnrate install
```

`burnrate install` writes the user service, enables it, and restarts it when it is already running (`systemctl --user restart` on Linux; `launchctl bootout`, then `bootstrap`, then `kickstart -k` on macOS). A configured Telegram token and chat id are written to `~/.config/burnrate/telegram.env` with mode 0600, or to `$XDG_CONFIG_HOME/burnrate/telegram.env` when that variable is set. The unit file does not contain the token. Running `burnrate install` again with no token removes that file.

On Linux, if the service should keep running without a login session: `loginctl enable-linger "$USER"`. On macOS the plist is `~/Library/LaunchAgents/com.artemimennoi.burnrate.plist`.

Try it once without installing the service:

```bash
burnrate status
burnrate forecast
```

Both commands scan local logs, then print. `burnrate agent` scans on a timer (default 30s), sends alerts, and answers the bot.

## Status line wrapper

Claude only includes `rate_limits` in the JSON it passes to the status line command. Point Claude Code at burnrate, and keep your existing script in the config.

`~/.claude/settings.json`:

```json
{
  "statusLine": {
    "type": "command",
    "command": "burnrate statusline"
  }
}
```

`~/.config/burnrate/config.toml`:

```toml
[claude]
statusline_command = "~/.claude/statusline.sh"
```

`burnrate statusline` starts `statusline_command` first, passing it the same stdin bytes, and prints that command's stdout. It records the window snapshot after the command has started, so the status line is not held behind the database write. Leave the command empty to record and print nothing. A nested `burnrate statusline`, including one reached through a wrapper script, records nothing and exits without printing. Do not set the command to `burnrate statusline`.

The command needs to be on `PATH` for Claude, or use an absolute path in `settings.json`.

## Telegram

Create a bot with [@BotFather](https://t.me/BotFather), open a chat with it, and send one message so the chat exists. Put the token and that chat id in the environment (preferred) or the config file:

```bash
export BURNRATE_TELEGRAM_TOKEN="123456:replace-me"
export BURNRATE_TELEGRAM_CHAT_ID="123456789"
```

The bot only replies to that chat id. Commands: `/status`, `/forecast`.

Alerts fire at 50%, 75%, and 90% of each window. A threshold is sent once per window (`resets_at`); the next window can alert again. The message has the provider, the window, the percent, the pace forecast, and the reset time in your timezone. It does not include prompts, paths, or token text.

If Telegram is not configured, the agent still records snapshots and logs how many alerts are waiting.

## Privacy

- No OAuth tokens, API keys, cookies, or `~/.codex/auth.json` are read or stored.
- No provider usage API is called. Claude percentages come only from the status line JSON you already receive.
- The SQLite file stays on the machine. Read cursors are keyed by a hash of the file path. Prompts and paths are not written to the database.
- Nothing leaves the machine unless Telegram is configured. Outbound text is the alert or the reply to `/status` and `/forecast`: provider, plan label, window, percent, pace, and reset time.
- Phase 3 aggregates are opt-in and not implemented. The default build has no phone-home.

## Roadmap

**Phase 2 (not computed yet).** The tables this version stores are the inputs.

- Pace versus a personal norm: for each finished window, percent per hour, then the median of those rates. Current pace is "faster than usual" when it sits above that median.
- Tokens per 1% of limit: between two snapshots of the same window, divide the token delta by the percent delta. A sharp drop means the same work consumes more of the allowance (a limit cut) rather than a faster session. Claude token rows are already stored; aligning them with status line snapshots is the remaining step.

Cursor and Grok would be opt-in collectors behind the same `provider.Collector` interface, using unofficial APIs. They are stubs and stay off.

**Phase 3 (not implemented).** Opt-in anonymized aggregates: a Cloudflare Worker and D1 accept `{provider, window, used_percent, plan_type, pace_per_hour}` with no account id, prompt, path, or hostname. A public page reads the rollups. Off unless a future config flag is set. This repo has no client for it.

## Comparison

| Tool | What it is for | How burnrate differs |
| --- | --- | --- |
| [ccusage](https://ccusage.com/) | Local token counts and estimated cost across many coding CLIs, including Claude Code and Codex. | burnrate tracks the subscription window percent and alerts on it. It is not a cost report. |
| [CodexBar](https://github.com/steipete/codexbar) | macOS menu bar (plus a CLI) for provider quotas and reset countdowns. Some providers are read via OAuth, cookies, or a CLI fallback. | One static binary for macOS and Linux. Codex comes from rollout files, Claude from the status line JSON. No credentials. Telegram alerts. |
| [Claude-Code-Usage-Monitor](https://github.com/Maciek-roboblog/Claude-Code-Usage-Monitor) | Terminal dashboard for Claude Code burn, with status line support for the official five-hour and weekly figures. | burnrate also reads Codex, runs in the background, and pushes alerts. It is not a TUI. |
| [ccwatch](https://pypi.org/project/ccwatch/) | Starts a Claude Code session and scrapes the `/usage` panel for threshold pings. | burnrate does not launch Claude or scrape the TUI. It reads the status line JSON and Codex logs. |

## Development

```bash
make ci     # go vet, go test, go test -race, static build
```

GitHub Actions runs `go vet`, `go test`, `go test -race` (CGO enabled for that job only), and a CGO-disabled build for darwin/linux on amd64 and arm64.

### Local fixture

```bash
mkdir -p "$HOME/.codex/sessions/2026/10/17"
reset=$(($(date +%s) + 18000))
printf '%s\n' "{\"timestamp\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\",\"type\":\"event_msg\",\"payload\":{\"type\":\"token_count\",\"rate_limits\":{\"limit_id\":\"codex\",\"plan_type\":\"plus\",\"primary\":{\"used_percent\":62,\"window_minutes\":300,\"resets_at\":$reset},\"secondary\":null}}}" \
  > "$HOME/.codex/sessions/2026/10/17/rollout-demo.jsonl"
burnrate status
```

A premium snapshot with null windows is ignored. A real `used_percent` of 0 is kept.

## License

MIT. Copyright 2026 artemimennoi.

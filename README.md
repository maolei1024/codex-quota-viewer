# Codex Quota Viewer

Read-only web dashboard for Cockpit Tools Codex quota cache.

The service does not call OpenAI and does not modify Cockpit Tools data. It reads
the mounted Cockpit data directory, masks account identities, and renders a
human-oriented dashboard.

When configured, it can also send a webhook notification when a Codex account's
weekly quota window resets.

## Data Sources

Mount the Cockpit Tools data directory as `/data:ro`:

- `/data/codex_accounts/*.json`
- `/data/codex_local_access_stats.json`
- `/data/codex_local_access_logs.sqlite`

`codex_local_access_stats.json` contains precomputed aggregate windows such as
totals, account totals, model totals, and API key totals. It does not retain the
raw event list, so it cannot answer model-by-account breakdowns by itself.

`codex_local_access_logs.sqlite` contains per-request local API service logs,
including timestamp, account id/email, API key id/label, model id, success
status, latency, token counts, and estimated cost. When this file is mounted,
the model request ranking includes a masked per-account breakdown for each
model row.

Codex quota data is only as fresh as Cockpit Tools' own automatic refresh. If
Cockpit Tools is not running, this viewer shows the last cached quota.

## Weekly Reset Notifications

The viewer can send a webhook when an account's weekly quota reset time is
reached, or when the cached weekly reset time jumps forward by at least five
minutes before the previous reset time was reached. The second case catches the
first early quota reset signal while ignoring small timestamp drift in cached
quota data.

Notifications are deduplicated per account and quota cycle. After a reset
notification, further forward movement of the next reset time is treated as the
same cycle until that next reset time is reached, so normal Codex usage does not
produce repeated reset notifications. The state is stored in
`WEEKLY_RESET_NOTIFY_STATE_DIR` so restarts do not resend the same reset
notification.

The default compose file sends JSON notifications to:

```text
https://mlntfy.project.k3s.ixuni.win/api/notifications/simple/send/mlNtfy
```

The JSON body uses:

- `title`: `Codex 周额度已重置`
- `priority`: `high`
- `tags`: `codex,quota,weekly-reset`
- `message`: masked account, weekly remaining percentage, observed reset time,
  and current next reset time

## Privacy

The dashboard and JSON APIs never return raw emails, account IDs, API key IDs,
tokens, refresh tokens, or raw quota payloads.

Email masking examples:

- `mike@gmail.com` -> `m***@**.com`
- `alice@company.co.uk` -> `a***@**.uk`
- `api-key-50ccfbb0` -> `api-key-****`

## Run With Docker Compose

```bash
mkdir -p "${HOME}/.antigravity_cockpit/codex_quota_viewer_state"

UID=$(id -u) GID=$(id -g) \
docker compose up -d
```

Default compose binding is local-only:

```yaml
ports:
  - "127.0.0.1:8080:8080"
```

Put Nginx or Caddy in front for HTTPS and Basic Auth when exposing it publicly.

The compose file runs the container as the host UID/GID so it can read
`~/.antigravity_cockpit` when that directory is `700`, while still keeping the
container non-root in practice.

## Configuration

Environment variables:

| Name | Default | Description |
| --- | --- | --- |
| `LISTEN_ADDR` | `:8080` | HTTP listen address inside the container |
| `DATA_DIR` | `/data` | Mounted Cockpit Tools data directory |
| `STALE_AFTER_MINUTES` | `30` | Mark quota cache as stale after this many minutes |
| `REFRESH_INTERVAL_SECONDS` | `300` | Dashboard refresh and weekly reset check interval |
| `WEEKLY_RESET_NOTIFY_URL` | empty | Webhook URL. Leave empty to disable reset notifications |
| `WEEKLY_RESET_NOTIFY_STATE_DIR` | `/state` | Writable directory for reset notification dedupe state |
| `WEEKLY_RESET_NOTIFY_TIMEOUT_SECONDS` | `10` | Webhook request timeout |

## Endpoints

- `GET /` - HTML dashboard
- `GET /api/summary` - full sanitized dashboard data
- `GET /api/accounts` - sanitized Codex account quota rows
- `GET /api/local-access/usage` - local API usage summary
- `GET /healthz` - health check

## Build Locally

```bash
go test ./...
docker build -t codex-quota-viewer:local .
```

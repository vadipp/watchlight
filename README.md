# Watchlight

Small self-hosted uptime monitoring in Go. Original frontend, SQLite storage,
YAML configuration, no administration forms, no running Node.js service.

**Early development version.** Run a primary instance on one host and an
independent watchdog on another. Use your own domains and HTTPS reverse proxy.

## Run locally

Requires Go 1.27.3 or newer (the current pinned SQLite dependency requires a recent
Go toolchain). Runtime has no external database dependency.

```sh
go build -trimpath -o watchlight .
cp config.example.yaml config.yaml
export WATCHLIGHT_PASSWORD='replace-with-a-long-random-password'
export WORKER_HEARTBEAT_TOKEN='replace-with-a-different-long-random-token'
./watchlight validate -config config.yaml
./watchlight serve -config config.yaml
```

Visit `http://127.0.0.1:8080/` for public status and `/dashboard` for the owner
view (HTTP Basic authentication; username `owner`). Production MUST use HTTPS
at the reverse proxy; keep the application port bound to loopback.
`validate` checks YAML structure and settings without needing secrets; `serve`
and `watchdog` require their configured secret environment variables to exist.
Do not put secrets in YAML, command arguments, URLs or Git.

## Supported checks

- `http`: GET, exact expected status (default 200), optional `body_contains`
  within the first 1 MiB. Normal HTTPS certificate verification. Redirects follow
  Go's standard client behavior. Ten-second default deadline covers the request.
- `tcp`: connect to `host:port` with a deadline; this verifies connectivity only.
- `heartbeat`: authenticated incoming GET or POST; see `docs/heartbeats.md`.

Each monitor has a stable `id`, display `name`, `public` visibility, `interval`,
`timeout`, `failures`, and `successes`. IDs preserve history through renames.
Changes to monitor configuration reset consecutive-result counters on the next
check. Changing an ID starts a new history. Unknown fields and duplicate IDs
are rejected. Changes take effect after validation and restart.

A failure threshold must be reached before going down; the success threshold
must be reached before recovery. An initial down event alerts; initial successful
startup does not. Missing scheduler progress appears as unknown, never healthy.
Historical chart gaps are shown between observations. The current UI shows the
latest 240 results, 24-hour observed success percentage and 30 recent transitions.
The history strip groups results into up to 60 blocks to fit the screen. A block
with any failed check is red; otherwise a monitoring gap makes it gray. Hover a
block for its time range and check/failure counts.
This percentage is sample-based, not a wall-clock availability SLA; unobserved
intervals do not count as successes.

## Notifications

Uncomment `telegram` in the example and provide the named token/chat variables.
Down and recovery transitions enter the SQLite outbox in the same transaction as
the incident. Failed deliveries retry with exponential backoff up to 512 seconds.
Delivery is at least once: a crash after acceptance can duplicate a message.
Only monitor display names and status go to Telegram, never target URLs or tokens.
Choose non-secret display names. Old queued transitions remain queued across
configuration edits; review the database before changing Telegram destinations.

The primary probes Telegram `getMe` every minute and exposes freshness/results
in the authenticated `/api/health` response, alongside scheduler and outbox health.
This tests API access and bot credentials, not delivery to the user's device.
Actual sends validate Telegram's HTTP and JSON success response.

## Independent watchdog

```sh
./watchlight validate -config watchdog.example.yaml
./watchlight watchdog -config watchdog.example.yaml
```

Run this on the watchdog host with its own local database and SMTP credentials. It checks
the primary's authenticated health endpoint, stale monitoring, notification backlog, and
the primary's Telegram probe. It separately probes Telegram from the watchdog host. Three failed
polls trigger an email when SMTP is configured; recovery triggers another. Watchdog state survives restart;
failed emails retry next poll. Email submission requires verified STARTTLS
(typically SMTP port 587); implicit TLS on port 465 is not supported. Use an
external mail provider independent of the primary. One recipient per configuration.

This is a single-watchdog design. A watchdog-host failure cannot be reported by that
watchdog itself. The primary should monitor the watchdog host as one of its regular checks.
Network isolation is reported as observer failure, not proof of host death.

## Data and backups

SQLite WAL mode, one serialized connection, restrictive created file permissions.
Results are committed once per check; no separate database process. Defaults:
seven days of raw results, hourly summaries and incidents retained for one year.
Hourly summaries are created transactionally before raw deletion; they store
counts, successes, latency sum/max. Long-term summaries are stored but not yet
charted. Cleanup runs on startup and hourly. SQLite reuses freed pages; retention
does not automatically shrink the file. Database and WAL need free disk space.

Stop the relevant service before copying its database for a simple consistent
backup, or use SQLite's online backup tooling. Never copy only the `.db` file
while writes are active; committed data may still be in `-wal`. No automatic
backup or schema upgrade command is included yet. Preserve the database across
binary updates. Config files and environment-secret files require separate backups.
Only one primary process may use a given database; do not share it across hosts.

## Development

```sh
go test ./...
go test -race ./...
go vet ./...
node --check web/app.js
```

No Uptime Kuma code, images or other assets have been copied. The UI uses native
browser JavaScript and CSS, embedded in the Go executable. No frontend build step.

Not yet included: maintenance scheduling, browser Web Push, notification resend
controls, long-term charts, bulk-import tools, detailed DNS/ICMP checks. Telegram and SMTP delivery require live credential testing
before deployment. Resource usage must be measured on target hardware before
setting production limits; Go alone is not a resource guarantee.

An optional real-binary smoke test is available as `node scripts/smoke.cjs`.
For browser checks, set `PLAYWRIGHT_MODULE` to an installed `playwright-core`
module and optionally `CHROMIUM_PATH` to a compatible Chromium executable.
The smoke test creates temporary local services, synthetic secrets and a temporary
database, and removes them afterward. Browser dependencies are development-only.

The watchdog may run before email is configured: omit `smtp` entirely for
observation-only operation. It persists state and logs transitions, but cannot
send independent fallback alerts until a complete SMTP configuration is added.

## Named public dashboards

Set `default_status_page` to redirect `/` to a configured page. Page membership
is explicit; each referenced monitor must also have `public: true`. Other
monitors stay private. For example:

```yaml
default_status_page: overview
status_pages:
  - slug: overview
    name: Service overview
    monitors: [example-web]
```

This serves `/status/overview` and its scoped JSON at `/api/status/overview`.
Unknown pages return 404. Without a default page, `/` retains the original public
view. `/api/public` retains the aggregate list of public monitors for compatibility.

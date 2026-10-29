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
  Set `record_latency: false` on HTTP monitors to discard new timings and hide
  response time and its chart in owner and public views (default: true). Existing
  history is retained but hidden while disabled. Uptime and incidents are unaffected.
  Disabled timings use zero placeholders in storage and history responses;
  `latency_ms` is null. This setting does not reset incident thresholds.
  Set `disable_keep_alive: true` to open a fresh connection for every HTTP check,
  including redirects. This avoids retries on stale pooled connections, but adds
  TCP/TLS connection setup to each measurement. Connection reuse is enabled by
  default. Changing this option preserves incident thresholds and history.
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
The grouped window selector offers 4 hours, 24 hours (default), and 7 days.
Observed uptime, history and response-time plots share that exact rolling window;
timestamps use dd.mm.yyyy and 24-hour local time.

Monitor lists at `/api/public`, authenticated `/api/private`, and scoped
`/api/status/{slug}` contain only IDs, names, current statuses and owner groups.
They read current state without querying history. Append `/{id}?window=4h`,
`24h` (default), or `7d` for one monitor's metrics, explicit window boundaries,
240 time buckets, recent status changes and permitted check details. Public detail
routes enforce visibility and page membership; owner details require authentication.
Raw check history is not returned.

The UI loads list, selected details and owner health independently, refreshing
about every 15 seconds without overlapping requests in each stream. Switching
monitor/window aborts obsolete detail requests. A bounded in-memory cache reuses
monitor/window details for 15 seconds; failures keep the list usable and label
previously loaded details as stale. Request contexts reach database queries and
aggregation, so disconnected clients can stop obsolete read work.

The strip combines buckets into up to 60 equal-duration bars. Each bar stacks
green (successful coverage), red (failed coverage), and grey (unknown time).
A check covers at most one configured interval, ending earlier if another check
arrives; remaining time, including missing history, is unknown. Hover, focus or
tap a bar for its interval, check/failure counts and coverage percentages.
Uptime remains the successful-check percentage, excluding unknown time; bar
segments are duration-based. This is not a wall-clock availability SLA.
Response-time points are bucket means. Recent status transitions retain their
separate threshold-based semantics and are not filtered by the window selector.
Raw retention shorter than the selected window appears as unknown coverage.

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

To monitor the watchdog itself, create a private `heartbeat` monitor on the
primary and configure `heartbeat_url` and `heartbeat_token_env` on the watchdog.
It POSTs the token in the Authorization header after each persisted check cycle,
including cycles that observe failures. The URL must use HTTPS and cannot contain
credentials, a query or a fragment; redirects are rejected. Delivery failures are
retried on the next cycle. Allow for probe/email timeouts when choosing `max_age`.
This measures watchdog liveness, not the health of the services it observes.

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

The UI uses native browser JavaScript and CSS, embedded in the Go executable.
No frontend build step.

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

## Owner dashboard groups

Set an optional `group` on each monitor to organize the authenticated dashboard.
Groups appear in configuration order, with collapsible service lists and status
counts. Ungrouped monitors appear under Other. Group names are omitted from public
APIs and status pages; grouping changes preserve existing check history and incident
thresholds. See `scripts/check-agent/README.md` for optional local probes that
report backup, certificate, telemetry or protocol health through private heartbeats.
The agent also supports one aggregate per Compose project, checking all required
services and reporting failed container identities only to the owner dashboard.

Within each owner group, down services appear before other services while their
relative configuration order is preserved. The bottom of the selected service's
owner view shows check type, target, timing, thresholds and expected response.
An optional monitor `description` explains an external heartbeat collector's
underlying probe. Descriptions and groups do not change check fingerprints.
Technical configuration, type and polling interval are omitted from both public
APIs; only the authenticated owner API includes the `check` object. Secret values
and secret environment names are never included in that object.

Response-time charts show a zero-based millisecond scale with labelled midpoint
and upper bound, subtle grid lines, and the observed minimum–maximum range. The
scale rounds up to a readable bound and retains precision below one millisecond.

## Hide a monitor until recovery

To temporarily remove a monitor from dashboards while continuing checks/history:

```sh
watchlight hide-until-ok -config config.yaml -id example-monitor
```

Run against the same database as the primary, with its normal filesystem permissions
(and the same data/config mounts when using Compose). The action is persisted in
SQLite and survives restarts. It excludes the monitor from public summary/detail
APIs and from owner group/dashboard counts. Owners can inspect it in a collapsed
Hidden checks section. Alerts are muted until consecutive fresh successful checks
meet the configured recovery threshold; earlier successes do not count. Recovery
atomically clears the snooze and queues one recovery/visibility notification when
notifications are enabled. Later failures behave normally. Repeat the command to
rearm the snooze. No configuration edit or service restart is needed for the action.

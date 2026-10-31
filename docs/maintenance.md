# Planned maintenance API

The authenticated API is the durable source of truth for explicitly planned
service interruptions. It accepts owner HTTP Basic credentials or
[scoped API tokens](api-tokens.md) restricted to explicit monitors and permissions. Credentials belong in protected files/headers, never URLs or Git. A monitor's
heartbeat token cannot create or read maintenance records.

## Start and complete

`PUT /api/maintenance/{monitor_id}/{operation_id}` with Content-Type
`application/json` starts an immediate window:

```json
{"reason":"Update the adapter","source":"development","duration_seconds":300}
```

Operation IDs are caller-assigned, 1–128 ASCII letters/digits/dots/dashes/underscores,
starting with a letter/digit. Use a unique ID for each operation. Duration is 1–86400
seconds. Reason (up to 512 UTF-8 bytes) and source (128 bytes) are required and
cannot contain control characters. Do not include secrets or private request data.
Server-assigned integer Unix seconds `starts_at` and `expires_at` prevent backdating.
The response is 201 on creation and 200 for an identical retry. Reusing an ID with
different parameters returns 409; retries never extend or reopen a window.

`POST /api/maintenance/{monitor_id}/{operation_id}/complete` with JSON `{}` ends
it early. `completed_at` is persisted and capped at the original expiry. Repeating
completion returns the same record. There is no edit, delete or extension route.
Multiple windows may overlap; coverage is their union.

Requests without valid authentication return 401; scoped grants can return 403; unknown monitors/operations 404;
invalid JSON or fields 400. Writes require JSON and reject browser cross-site
requests (415). No CORS access is enabled. Storage failures return 503. Do not
assume a maintenance window exists after a failed/uncertain write: retry the same
operation and parameters or read it back before stopping a service.

## Read and classify

`GET /api/maintenance/{monitor_id}?start=UNIX_SECONDS&end=UNIX_SECONDS` returns
records intersecting the half-open interval `[start,end)`, ordered by start.
Defaults cover the last day; explicit queries may cover at most 90 days. History
persists in SQLite across process restarts. The effective end is
`completed_at` when present, otherwise `expires_at`.

A checker should query the exact observation period, rather than asking only if
maintenance is active now. Classify an interruption as planned only to the extent
it overlaps recorded coverage for that monitor. A failure before the start or
after the effective end remains unexpected. A marker records intent, not proof
of the actual stop cause or successful recovery. Do not infer planned maintenance
solely from a graceful shutdown or a familiar time of day. API failure means the
classification is unknown, not that an outage is planned.

Start the window only after validation, immediately before stopping the service.
Complete only after independently verifying service recovery (for a heartbeat
monitor, fresh sender heartbeats and recovered status). Do not close a window
unconditionally in an error trap: expiry is the bounded fallback. Backup jobs can
use `source: backup` around their actual service-stop/recovery phase rather than
the full upload. Development uses the same API with `source: development`.

## Checks, alerts and dashboard

Checks, raw results, threshold state and incident transitions continue throughout
maintenance. New transition alerts are muted while any window is active; existing
queued notifications are retained. A durable separate alert baseline ensures a
still-down service alerts on the next check after completion/expiry, even if its
down transition happened during maintenance and the primary restarted. Normal
thresholds still apply. A fully recovered maintenance interruption creates no
unmatched recovery alert. An incident already alerted before maintenance is not
erased; its recovery can be delivered once the window ends.

Public summaries expose only a maintenance boolean; public details expose active
state and maintenance duration per bucket. Private reason/source/operation IDs
are restricted to authenticated owner or explicitly scoped maintenance reads. Owner details include the intersecting records.
Public visibility and status-page membership still apply.

The segmented strip keeps all successful coverage green, including inside
maintenance windows. Blue (#388bfd) replaces only failed or unknown coverage
inside maintenance; failures outside maintenance remain red and other unknown
time remains grey. Overlapping windows never double count. The blue duration
therefore measures non-successful coverage during maintenance, not the full
maintenance window. A two-pixel minimum keeps a short blue segment visible in wide windows;
hover/focus/tap percentages show the actual duration share. Raw failure counts
and observed uptime remain unmodified, including checks during maintenance; the
strip is duration-based, while uptime is check-based. This distinction is
intentional: maintenance explains downtime without rewriting the measurements.

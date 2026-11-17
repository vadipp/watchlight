# Local check agent

Optional Python 3 standard-library agent for private heartbeat monitors. It runs
bounded read-only probes once a minute using the supplied systemd units. It opens
no listener, runs no web server, and needs no Watchlight owner password.

Keep its JSON configuration and tokens outside Git, root-owned mode 0600. Install
`agent.py` under `/usr/local/lib/watchlight-checks/`, the configuration under
`/etc/watchlight-checks/config.json`, and create `/var/lib/watchlight-checks` for
optional probe state. Review the systemd sandbox against your chosen commands.
Only the heartbeat credential for each assigned monitor is needed on a host.

```json
{
  "endpoint": "https://status.example.com",
  "checks": [
    {
      "id": "daily-backup",
      "token": "REPLACE_WITH_A_PRIVATE_HEARTBEAT_TOKEN",
      "kind": "timestamp",
      "path": "/var/lib/backups/last-success.json",
      "field": "finished",
      "require": {"success": true},
      "max_age": 93600
    }
  ]
}
```

Each successful probe POSTs its token in the Authorization header. Failed probes POST a structured error code and, for HTTP status failures, the numeric
status. Fresh failures use the primary failure threshold. Missing reports separately
expire after the freshness window. Configure `max_age` on the primary to tolerate agent scheduling and probe
runtime (five minutes is a reasonable starting point for a one-minute timer).
Probe failures log IDs and sanitized reports; delivery failures are logged separately.
Neither includes command output,
URLs, exception details or credentials. IDs must be non-secret.

Probe kinds: `docker` (all named containers running, not paused/restarting/dead,
and healthy when a Docker health check exists), `http` (method, headers, optional body/status/contains), `command`
(argv, optional timeout/contains), `timestamp` (ISO/Unix timestamp or JSON field
with required result fields), `file_age` (mtime and marker text), `systemd`
(successful recent completion in this boot), `certificate` (verified live TLS,
minimum remaining days), `certificate_file` (local PEM remaining days), `query`
(fresh nonempty VictoriaMetrics/Prometheus vector and numeric thresholds), and
`any` (at least one successful evidence source). Query freshness should check
`time() - timestamp(metric)` explicitly to detect stopped ingestion despite query
lookback. Commands run without a shell; timed-out process groups are killed.

`endpoint_command` can resolve a changing local container endpoint. Plain HTTP
requires explicit `allow_private_http: true` and must be confined to a trusted
local/private network. HTTPS uses system certificate verification. Redirects and
environment HTTP proxies are disabled. Agent commands and configuration are
trusted operator input; do not accept them from web requests.

Use `agent.py /path/to/config.json --dry-run` to run probes without sending any
heartbeats. Run `python3 scripts/check-agent/test_agent.py` for synthetic failure,
freshness, authentication and timeout checks. The agent does not schedule backups,
run restoration, renew certificates, or repair failing applications.

## One aggregate per Compose project

Use `compose` with an explicit list of required long-running services:

```json
{
  "id": "example-stack",
  "token": "REPLACE_WITH_A_PRIVATE_HEARTBEAT_TOKEN",
  "kind": "compose",
  "project": "example",
  "services": ["api", "worker", "db"]
}
```

The agent resolves current containers by Compose project/service labels on every
run, including stopped containers. Every required service must have at least one
container; all discovered replicas of those services must be running, unpaused,
not restarting/dead, and healthy whenever Docker health checks exist. Recreated
container names are discovered automatically. This checks existing replicas;
it does not verify a configured desired replica count after a replica disappears.
Keep one-shot jobs, optional profiles and deliberately stopped services outside
the required list. Review membership when adding new services: the explicit
required list is not automatically extended. Standalone containers can use the
existing `docker` probe with an explicit `containers` list.

Known container failures send a structured failure report with only names and
enumerated states. Successful checks send an empty heartbeat to clear failures.
Docker access/inspection failures send structured probe errors. All probes report
failures; expiry detects missing collector or delivery evidence.
Upgrade the primary to support structured reports before upgrading collectors;
older primaries ignore bodies and would treat a failure report as a success.
The owner dashboard shows failed members. No failure details enter public APIs.

Aggregate checks complement functional HTTP/protocol checks. A running container
without a Docker health check only proves process liveness. The agent neither
restarts containers nor changes Compose projects.

General errors carry only an enumerated code and optional numeric HTTP status.
Codes cover HTTP status, DNS, TLS, timeout, connection, content, command, stale
evidence, certificate expiry, metrics, file and unclassified probe failures.
Raw exceptions, URLs, headers, response bodies and command output are excluded.

## Additional functional probes

- `dns_udp`: `host`, `port`, `name`; sends an A query with a random transaction ID
  and requires a matching successful, non-truncated response with an answer.
- `mqtt`: `host`, `port`, optional `username`/`password`; authenticates and performs
  a PING round trip. With `zigbee_base_topic`, it instead subscribes and requests
  Zigbee2MQTT's read-only bridge health check, requiring a fresh matching
  transaction and `healthy: true`. It does not control devices.
- `transport`: `argv`; runs the bounded Go helper in `tools/transport-probe` and
  forwards its structured error category or HTTP status. This supports real
  HTTP/3 with no TCP fallback and authenticated HTTP/HTTPS CONNECT proxies.

An `http` probe may use `container`, `network`, `port` and `path` instead of `url`.
It resolves the current Docker address each cycle without reading environment
variables. `bearer_token_file` reads an existing local token directly into the
Authorization header; credential files need not move between hosts.

## Metrics export

Optional top-level `metrics_host` and `metrics_path` enable an atomic Influx line
protocol snapshot after each completed cycle. The path must be inside the agent's
allowed writable state directory. A Telegraf `inputs.file` with `data_format =
"influx"` can ingest it through the existing host-filesystem mount.

Measurements are `watchlight_collection` (completion timestamp), `watchlight_probe`
(per-check success including delivery, and total round-trip duration), and
`watchlight_host` (kernel OOM counter and available CPU/memory/I/O pressure data).
No credentials, request URLs, response bodies or command output become labels.
Pressure metrics are omitted on kernels without the pressure interface. Kernel
OOM counters include killed child processes even when Docker's OOM flag is false.

Optional `nginx_log` aggregates a dedicated JSON-lines log with only `server`,
`status`, `bytes`, and `duration`. It reads at most 4 MiB per cycle, persists
incremental counters beside the metrics file, and emits `nginx_requests` totals
for requests, 5xx responses, bytes and duration. Configure rotation for that log.
Aggregation is diagnostic: an unread rotation tail can be missed. URLs and client
addresses must be excluded from the source log. Dry runs do not export metrics.

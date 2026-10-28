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

Each successful probe POSTs its token in the Authorization header. General failed probes
send no heartbeat; the primary declares them down after their freshness/threshold
window. Configure `max_age` on the primary to tolerate agent scheduling and probe
runtime (five minutes is a reasonable starting point for a one-minute timer).
Probe failures and delivery failures log only monitor IDs, never command output,
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
Docker access/inspection failures send no heartbeat; expiry still detects the
loss of evidence. General probes continue sending heartbeats only on success.
Upgrade the primary to support structured reports before upgrading collectors;
older primaries ignore bodies and would treat a failure report as a success.
The owner dashboard shows failed members. No failure details enter public APIs.

Aggregate checks complement functional HTTP/protocol checks. A running container
without a Docker health check only proves process liveness. The agent neither
restarts containers nor changes Compose projects.

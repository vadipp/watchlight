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

Each successful probe POSTs its token in the Authorization header. Failed probes
send no heartbeat; the primary declares them down after their freshness/threshold
window. Configure `max_age` on the primary to tolerate agent scheduling and probe
runtime (five minutes is a reasonable starting point for a one-minute timer).
Probe failures and delivery failures log only monitor IDs, never command output,
URLs, exception details or credentials. IDs must be non-secret.

Probe kinds: `http` (method, headers, optional body/status/contains), `command`
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

# Scoped API tokens

Automation can use `Authorization: Bearer TOKEN` without receiving the owner
password. Configure explicit grants in YAML; keep random secrets (at least 32
characters, recommended 32 random bytes) in protected environment files. Secrets
must differ from owner, heartbeat and other API credentials. Never put them in
URLs, command arguments, logs or Git.

```yaml
api_tokens:
  - name: nightly-backup
    token_env: BACKUP_MAINTENANCE_TOKEN
    monitors: [worker-heartbeat]
    scopes: [maintenance:read, maintenance:write, monitor:read]
    operation_prefix: backup-
    max_duration_seconds: 900
```

There are no implicit grants or wildcards. The supported scopes are:

| Scope | Allowed routes |
| --- | --- |
| `maintenance:read` | `GET /api/maintenance/{id}` for allowlisted monitors |
| `maintenance:write` | Start/complete maintenance for allowlisted monitors and operation prefix |
| `monitor:read` | `GET /api/monitors/{id}/status` for allowlisted monitors |

Write grants require an operation prefix (1–96 operation-ID characters) and a
maximum duration (1–86400 seconds). Operation IDs must start with the prefix
and contain a suffix. A write grant permits completing any operation in that
namespace; assign distinct prefixes to independent automations. It does not
permit extending, backdating or deleting records. Reading history requires its
own scope. Owner Basic authentication retains access to these routes.

The recovery status response contains only `id`, `status`, `checked`,
`heartbeat_received_at` (zero for other check types), and `server_time`, with
integer Unix seconds. Stale checks report `unknown`. A running heartbeat sender
is recovered only after a fresh heartbeat has been observed by a successful
check. Use server time read after restarting the service as a recovery barrier;
a heartbeat received before stopping must not count as recovery.

API tokens cannot access the dashboard, private monitor details/lists, health,
snoozing, or heartbeat ingestion. Heartbeat tokens cannot use these grants.
Unknown/invalid credentials return 401; valid tokens outside their monitor,
scope, operation namespace or duration limit return 403. JSON and browser
cross-site protections still apply to writes. HTTPS is required in production.

Validate configuration with `watchlight validate`; startup also validates secret
strength and separation. Add, rotate or revoke grants through configuration and
restart the primary. Existing secrets remain in memory until restart. Remove the
grant to revoke it; rotate by changing its secret environment value. Existing
maintenance history survives revocation and expires as originally recorded.

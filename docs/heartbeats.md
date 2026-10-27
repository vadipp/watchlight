# Incoming heartbeat monitors

For services behind NAT, private Home Assistant installations, backup jobs or
anything that should report in instead of accepting probes.

```yaml
monitors:
  - id: home-assistant
    name: Home Assistant
    type: heartbeat
    public: false
    token_env: HA_HEARTBEAT_TOKEN
    interval: 30s
    timeout: 10s
    max_age: 2m
    failures: 3
    successes: 1
```

Set `HA_HEARTBEAT_TOKEN` to a unique random value on the monitoring server.
The sender uses the same value in an Authorization header:

```http
POST /api/heartbeat/home-assistant HTTP/1.1
Host: status.example.com
Authorization: Bearer <token>
```

GET is also supported with the same header. Both return HTTP 204 only after the
arrival time is committed. Tokens in URL query parameters are not supported;
URLs commonly end up in logs. Use HTTPS. Heartbeat tokens cannot access the owner
dashboard or update another monitor. An accepted request means the sender reported
in; it does not certify the sender's internal dependencies.

`max_age` is how long the most recent arrival remains valid. `interval` is how
often Watchlight evaluates that age. With the above settings and regular 60-second
pings, an outage is confirmed roughly 3–3.5 minutes after the last arrival
(2-minute age plus three failed 30-second evaluations). The first valid ping
recovers at the next evaluation. No ping after a new installation is a failure,
not an indefinitely healthy grace period. Arrival timestamps survive restarts.

## Home Assistant example

Merge these sections into existing configuration; do not duplicate existing root
keys. Store the complete `Bearer ...` string in Home Assistant's `secrets.yaml`:

```yaml
# secrets.yaml — private, never commit
watchlight_heartbeat_authorization: "Bearer <same-random-token>"
```

```yaml
rest_command:
  watchlight_heartbeat:
    url: "https://status.example.com/api/heartbeat/home-assistant"
    method: POST
    headers:
      Authorization: !secret watchlight_heartbeat_authorization
    timeout: 10

automation:
  - id: watchlight_heartbeat
    alias: Watchlight heartbeat
    triggers:
      - trigger: time_pattern
        minutes: "/1"
      - trigger: homeassistant
        event: start
    actions:
      - action: rest_command.watchlight_heartbeat
```

This is a proposed integration example, not an applied change to Home Assistant.
Validate against the installed HA version before applying. Rotate a token by
updating both ends and restarting Watchlight; do not reuse owner credentials.

REST command method, header and action syntax follow the official
[Home Assistant RESTful Command documentation](https://www.home-assistant.io/integrations/rest_command/).

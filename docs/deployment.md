# Deployment guide

Run the primary monitor and watchdog on separate hosts with independent local
storage. Both modes use the same binary, with different YAML configuration.

## Primary

- Use a dedicated unprivileged user and persistent local data directory.
- Keep the HTTP listener on loopback and terminate HTTPS at a reverse proxy.
- Serve the public status page at your chosen domain, such as status.example.com.
  Optional aliases can serve the same application. Avoid redirecting heartbeat
  POSTs between domains.
- Preserve Authorization headers through the proxy. Do not log them or request
  bodies. Keep tokens in headers and secret files outside the repository.
- Configure a supervisor to restart the process after failure. Allow a graceful
  SIGTERM shutdown before force-killing it.
- Back up SQLite consistently along with configuration and protected secrets.

## Watchdog

- Use a separate local database and an external SMTP provider independent of the
  primary host. Set SMTP and primary-authentication secrets in the environment.
- Check the primary's public HTTPS health endpoint to include ingress failures.
- Configure the primary to monitor watchdog host connectivity too.
- The watchdog does not expose a web server or need incoming public ports.

## Verification before relying on alerts

- Exercise HTTP failures and recoveries with a disposable monitor.
- Test actual Telegram message submission to the intended recipient.
- Send authenticated GET/POST heartbeats, stop the sender, and verify timeout and
  recovery notifications.
- Stop the primary and verify watchdog email. Test Telegram failure independently
  using a test process without disturbing unrelated networking.
- Measure CPU, RSS and disk writes at the intended monitor count before setting
  process limits. Restore a database backup and verify incident continuity.

See the README for current limitations and supported configuration options.

## Docker Compose

Build once with `docker compose build`. Copy `config.example.yaml` to
`config.yaml`; for containers set `listen: 0.0.0.0:8080` and
`database: /data/watchlight.db`. Put the referenced secret variables in `.env`
(mode 0600, excluded from Git). Start with `docker compose up -d`. The host port
is loopback-only; use a reverse proxy for public HTTPS. Containers run without
root, with a read-only root filesystem and persistent named-volume storage.

The separate watchdog file supports legacy Docker Compose 1.x as well:
`docker-compose -f compose.watchdog.yaml up -d`. Load/build the same image on that
host first. Its `config.yaml` should follow `watchdog.example.yaml`, with
`database: /data/watchdog.db`. It publishes no ports.

The scratch runtime contains the executable and CA certificates. Rebuild images
to update either. Database storage is independent of image updates. Resource caps
are starting limits, not promises of supported capacity; measure your workload.
Do not run `docker compose down -v` when preserving monitoring history.

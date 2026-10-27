# Local validation — 2026-10-27

This is development-machine evidence, not a production capacity estimate.

- Go tests exercise transitions, durable outbox across reopen, transactional
  rollback, public/private isolation, authenticated dashboard, stale scheduler,
  GET/POST heartbeat authentication, heartbeat expiry/recovery, HTTP status/body
  matching, strict YAML validation, rollup idempotency, target changes and watchdog
  detection of notification backlog.
- `go vet ./...` and frontend JavaScript syntax checks pass.
- A real local binary with an HTTP fixture and heartbeat sender passes access
  isolation and heartbeat expiry/recovery checks. A two-monitor smoke run sampled
  resident memory at 11,460 KiB. No Telegram or SMTP credentials were used.
- A preliminary 1,000-transaction SQLite benchmark measured 2.78 ms per record
  before the configuration fingerprint allocation optimization. It does not
  include network checks, UI load, or target-host storage behavior.
- The race detector could not run in this environment: CGO is disabled and no C
  compiler is installed. Run `CGO_ENABLED=1 go test -race ./...` in a prepared
  development environment before production deployment.

Live Telegram delivery, SMTP delivery, production ingress/certificates, backup
restore, sustained-load resource measurements and fault injection on actual
hosts remain deployment verification work. No live hosts have been changed.

Headless Chromium checks also pass for public/private dashboard contents, service
selection, JavaScript execution and absence of horizontal overflow at a 390-pixel
viewport. The final two-monitor smoke run sampled 12,340 KiB RSS. These functional
browser checks are not a full visual review or sustained-load benchmark.

# Transport probe

Bounded HTTP/3 and authenticated HTTPS-forward-proxy client for the check agent's
`transport` probe. Build with `go build .` here. Requests have a twelve-second
deadline, 64 KiB body limit and normal TLS verification. HTTP/3 has no TCP fallback.

Store config outside Git, mode 0600:

```json
{"mode":"http3","url":"https://probe.example.com/probe","contains":"HTTP/3.0"}
```

Proxy mode adds `mode: proxy`, `proxy_url` (HTTP or HTTPS), `username`, `password`,
and an HTTPS target `url`. Credentials are attached in memory; URL userinfo is
rejected. Invoke `transport-probe -config /protected/probe.json`. Stdout contains
only structured `ok`/`error` results understood by the collector. Targets,
credentials, raw errors and response bodies are never logged.

An optional `ca_file` augments system trust with a local PEM bundle; it never
disables certificate or hostname validation. This also permits isolated TLS and
HTTP/3 integration tests without contacting external services.

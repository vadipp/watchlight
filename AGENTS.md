# Watchlight development

Keep monitor history, secrets, runtime configuration and generated binaries out
of Git. Examples and documentation must use generic names and reserved example
domains. Never include a contributor's infrastructure details or credentials.
This includes test fixtures, group names, monitor inventories, screenshots and
assertions: invent generic examples instead of copying live configuration or API
responses. Review the staged diff for private deployment data before each push.

Go application and vanilla embedded frontend. Keep dependencies and resource
usage modest. Test incident transitions, persistence, access separation and
watchdog failure handling when modifying those paths. Incoming heartbeat tokens belong in headers, never
URLs. Avoid adding administration screens without a clear product requirement.

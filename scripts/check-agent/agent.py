#!/usr/bin/env python3
"""Run bounded local probes and report private heartbeat/container evidence.

Configuration/tokens belong in a root-only JSON file, never in the source tree.
Probe stdout/stderr and exception details are intentionally never logged.
"""
import concurrent.futures
import datetime
import json
import math
import os
import re
import signal
import ssl
import socket
import subprocess
import sys
import time
import urllib.parse
import urllib.request
import urllib.error


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


HTTP = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())


class ContainerFailure(ValueError):
    def __init__(self, failures):
        super().__init__("container probe failed")
        self.failures = failures


def container_status(state):
    for flag, status in [("Paused", "paused"), ("Restarting", "restarting"), ("Dead", "dead")]:
        if state.get(flag):
            return status
    if not state.get("Running"):
        status = state.get("Status", "not-running")
        return status if status in ("exited", "created", "removing") else "not-running"
    if "Health" in state and state["Health"].get("Status") != "healthy":
        return "starting" if state["Health"].get("Status") == "starting" else "unhealthy"
    return None


def compose_probe(c):
    project, services = c["project"], c["services"]
    if not re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}", project) or not services or any(
            not re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}", service) for service in services):
        raise ValueError("invalid compose membership")
    # Include stopped containers. Query only identity, membership and state;
    # never read Docker environment values or authentication-bearing labels.
    output = command(["docker", "ps", "-a", "--filter", "label=com.docker.compose.project=" + project,
                      "--format", "{{.ID}}"])
    ids = output.decode().split()
    rows = []
    if ids:
        if len(ids) > 256:
            raise ValueError("too many containers")
        output = command(["docker", "inspect", "--format",
                          '{{json .Name}} {{json (index .Config.Labels "com.docker.compose.service")}} {{json .State}}', *ids])
        decoder = json.JSONDecoder()
        for line in output.decode().splitlines():
            row = []
            for _ in range(3):
                value, end = decoder.raw_decode(line.lstrip())
                row.append(value)
                line = line.lstrip()[end:]
            rows.append(row)
    failures = []
    for service in services:
        members = [(name.lstrip("/"), state) for name, member, state in rows if member == service]
        if not members:
            failures.append({"name": service, "status": "missing"})
        for name, state in members:
            status = container_status(state)
            if status:
                failures.append({"name": name, "status": status})
    if failures:
        if len(failures) > 64:
            raise ValueError("too many failures")
        raise ContainerFailure(failures)


def request(url, method="GET", headers=None, body=None, status=200):
    req = urllib.request.Request(url, data=body, headers=headers or {}, method=method)
    try:
        response = HTTP.open(req, timeout=15)
    except urllib.error.HTTPError as error:
        if error.code != status:
            raise
        response = error
    with response:
        if response.status != status:
            raise ValueError("unexpected status")
        return response.read(1024 * 1024)


def timestamp(value):
    if isinstance(value, (int, float)):
        return float(value)
    return datetime.datetime.fromisoformat(value.strip().replace("Z", "+00:00")).timestamp()


def fresh(value, max_age):
    age = time.time() - timestamp(value)
    if not 0 <= age <= max_age:
        raise ValueError("stale or future timestamp")


def command(argv, timeout=25):
    proc = subprocess.Popen(argv, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, start_new_session=True)
    try:
        output, _ = proc.communicate(timeout=timeout)
        if proc.returncode:
            raise ValueError("command failed")
        return output
    except subprocess.TimeoutExpired:
        os.killpg(proc.pid, signal.SIGKILL)
        proc.communicate()
        raise


def probe(c):
    kind = c["kind"]
    if kind == "any":
        for candidate in c["checks"]:
            try:
                probe(candidate)
                return
            except Exception:
                pass
        raise ValueError("no fresh successful evidence")
    elif kind == "command":
        output = command(c["argv"], c.get("timeout", 25))
        if c.get("contains", "").encode() not in output:
            raise ValueError("expected output missing")
    elif kind == "docker":
        names = c["containers"]
        if not names or any(not name or name.startswith("-") for name in names):
            raise ValueError("invalid container names")
        output = command(["docker", "inspect", "--format", "{{json .State}}", *names])
        states = [json.loads(line) for line in output.decode().splitlines()]
        if len(states) != len(names):
            raise ValueError("missing containers")
        failures = [{"name": name, "status": container_status(state)} for name, state in zip(names, states) if container_status(state)]
        if failures:
            raise ContainerFailure(failures)
    elif kind == "compose":
        compose_probe(c)
    elif kind == "http":
        output = request(c["url"], c.get("method", "GET"), c.get("headers"),
                         c.get("body", "").encode() if "body" in c else None, c.get("status", 200))
        if c.get("contains", "").encode() not in output:
            raise ValueError("expected response missing")
    elif kind == "timestamp":
        with open(c["path"]) as f:
            value = f.read().strip()
        if "field" in c:
            value = json.loads(value)
            for key, expected in c.get("require", {}).items():
                if value.get(key) != expected:
                    raise ValueError("result not successful")
            value = value[c["field"]]
        fresh(value, c["max_age"])
    elif kind == "file_age":
        fresh(os.stat(c["path"]).st_mtime, c["max_age"])
        with open(c["path"]) as f:
            if c.get("contains", "") not in f.read(65536):
                raise ValueError("verification marker missing")
    elif kind == "systemd":
        output = command(["systemctl", "show", c["unit"], "-p", "Result", "-p", "ExecMainExitTimestampMonotonic"])
        fields = dict(line.split("=", 1) for line in output.decode().splitlines() if "=" in line)
        if fields.get("Result") != "success":
            raise ValueError("unit failed")
        finished = int(fields.get("ExecMainExitTimestampMonotonic", 0)) / 1e6
        if not finished or not 0 <= time.clock_gettime(time.CLOCK_BOOTTIME) - finished <= c["max_age"]:
            raise ValueError("unit missing or stale")
    elif kind == "certificate":
        with socket.create_connection((c["host"], c.get("port", 443)), timeout=10) as conn:
            with ssl.create_default_context().wrap_socket(conn, server_hostname=c["host"]) as tls:
                expiry = ssl.cert_time_to_seconds(tls.getpeercert()["notAfter"])
                if expiry - time.time() < c.get("days", 21) * 86400:
                    raise ValueError("certificate expiring")
    elif kind == "certificate_file":
        command(["openssl", "x509", "-in", c["path"], "-noout", "-checkend", str(c.get("days", 21) * 86400)])
    elif kind == "query":
        url = c["url"] + "/api/v1/query?" + urllib.parse.urlencode({"query": c["query"]})
        response = json.loads(request(url))
        if response.get("status") != "success":
            raise ValueError("query failed")
        values = response["data"]["result"]
        if len(values) != c.get("count", 1):
            raise ValueError("missing or unexpected series")
        for item in values:
            sample_time, value = item["value"]
            fresh(sample_time, c.get("max_age", 120))
            value = float(value)
            if not math.isfinite(value) or not c.get("min", -math.inf) <= value <= c.get("max", math.inf):
                raise ValueError("metric outside threshold")
    else:
        raise ValueError("unsupported probe")


def run_one(c, base, dry_run):
    try:
        probe(c)
        if not dry_run:
            request(base + "/api/heartbeat/" + urllib.parse.quote(c["id"], safe=""),
                    method="POST", headers={"Authorization": "Bearer " + c["token"]}, status=204)
        print(c["id"] + ": ok", flush=True)
        return True
    except ContainerFailure as error:
        if not dry_run:
            try:
                request(base + "/api/heartbeat/" + urllib.parse.quote(c["id"], safe=""),
                        method="POST", headers={"Authorization": "Bearer " + c["token"], "Content-Type": "application/json"},
                        body=json.dumps({"failures": error.failures}).encode(), status=204)
            except Exception:
                pass
        print(c["id"] + ": failed", flush=True)
        return False
    except Exception:
        print(c["id"] + ": failed", flush=True)
        return False


def main():
    with open(sys.argv[1]) as f:
        config = json.load(f)
    dry_run = "--dry-run" in sys.argv[2:]
    base = config["endpoint"]
    if config.get("endpoint_command"):
        base = command(config["endpoint_command"]).decode().strip()
    parsed = urllib.parse.urlparse(base)
    if parsed.scheme not in ("https", "http") or parsed.username or parsed.query or parsed.fragment:
        raise ValueError("invalid heartbeat endpoint")
    # HTTP endpoints must be explicitly opted into for an isolated local network.
    if parsed.scheme != "https" and not config.get("allow_private_http", False):
        raise ValueError("HTTPS required")
    with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
        results = list(pool.map(lambda c: run_one(c, base.rstrip("/"), dry_run), config["checks"]))
    return 0 if all(results) else 1


if __name__ == "__main__":
    sys.exit(main())

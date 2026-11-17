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
import struct
import uuid
import pathlib
import ipaddress


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


HTTP = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())


class ContainerFailure(ValueError):
    def __init__(self, failures):
        super().__init__("container probe failed")
        self.failures = failures


class ProbeFailure(ValueError):
    def __init__(self, code, http_status=None):
        self.report = {"code": code}
        if http_status is not None:
            self.report["http_status"] = http_status


def failure_report(error):
    # Never transmit raw exceptions: they can contain credentials, URLs or output.
    if isinstance(error, ProbeFailure):
        return error.report
    if isinstance(error, urllib.error.HTTPError):
        return {"code": "http_status", "http_status": error.code}
    if isinstance(error, urllib.error.URLError):
        return failure_report(error.reason)
    for kind, code in [(socket.gaierror, "dns"), (ssl.SSLError, "tls"),
                       (TimeoutError, "timeout"), (subprocess.TimeoutExpired, "timeout"),
                       (ConnectionError, "connection"), (FileNotFoundError, "file"),
                       (PermissionError, "file"), (OSError, "connection")]:
        if isinstance(error, kind):
            return {"code": code}
    return {"code": "probe"}


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
            raise ProbeFailure("http_status", response.status)
        return response.read(1024 * 1024)


def timestamp(value):
    if isinstance(value, (int, float)):
        return float(value)
    return datetime.datetime.fromisoformat(value.strip().replace("Z", "+00:00")).timestamp()


def fresh(value, max_age):
    age = time.time() - timestamp(value)
    if not 0 <= age <= max_age:
        raise ProbeFailure("stale")


def command(argv, timeout=25):
    proc = subprocess.Popen(argv, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, start_new_session=True)
    try:
        output, _ = proc.communicate(timeout=timeout)
        if proc.returncode:
            raise ProbeFailure("command")
        return output
    except subprocess.TimeoutExpired:
        os.killpg(proc.pid, signal.SIGKILL)
        proc.communicate()
        raise


def probe(c):
    kind = c["kind"]
    if kind == "transport":
        result = json.loads(command(c["argv"]))
        if result.get("ok") is not True:
            error = result.get("error", {})
            code = error.get("code", "probe")
            if code not in ("http_status", "dns", "tls", "timeout", "connection", "content", "probe"):
                code = "probe"
            raise ProbeFailure(code, error.get("http_status") if code == "http_status" else None)
    elif kind == "dns_udp":
        ident = os.urandom(2)
        name = b"".join(bytes([len(p)]) + p.encode("ascii") for p in c["name"].rstrip(".").split(".")) + b"\0"
        query = ident + struct.pack("!HHHHH", 0x0100, 1, 0, 0, 0) + name + struct.pack("!HH", 1, 1)
        with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as conn:
            conn.settimeout(8)
            conn.connect((c["host"], c["port"]))
            conn.send(query)
            response = conn.recv(4096)
        if len(response) < 12 or response[:2] != ident:
            raise ProbeFailure("content")
        flags, _, answers, _, _ = struct.unpack("!HHHHH", response[2:12])
        if not flags & 0x8000 or flags & 0x020f or not answers:
            raise ProbeFailure("dns")
    elif kind == "mqtt":
        mqtt_probe(c)
    elif kind == "any":
        for candidate in c["checks"]:
            try:
                probe(candidate)
                return
            except Exception:
                pass
        raise ProbeFailure("evidence")
    elif kind == "command":
        output = command(c["argv"], c.get("timeout", 25))
        if c.get("contains", "").encode() not in output:
            raise ProbeFailure("content")
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
        url = c.get("url")
        if c.get("container"):
            network = c["network"]
            if not re.fullmatch(r"[a-zA-Z0-9_.-]+", network):
                raise ProbeFailure("probe")
            template = '{{(index .NetworkSettings.Networks "' + network + '").IPAddress}}'
            address = command(["docker", "inspect", "--format", template, c["container"]]).decode().strip()
            ipaddress.IPv4Address(address)
            url = "http://" + address + ":" + str(c["port"]) + c["path"]
        headers = dict(c.get("headers", {}))
        if c.get("bearer_token_file"):
            headers["Authorization"] = "Bearer " + pathlib.Path(c["bearer_token_file"]).read_text().strip()
        output = request(url, c.get("method", "GET"), headers,
                         c.get("body", "").encode() if "body" in c else None, c.get("status", 200))
        if c.get("contains", "").encode() not in output:
            raise ProbeFailure("content")
    elif kind == "timestamp":
        with open(c["path"]) as f:
            value = f.read().strip()
        if "field" in c:
            value = json.loads(value)
            for key, expected in c.get("require", {}).items():
                if value.get(key) != expected:
                    raise ProbeFailure("evidence")
            value = value[c["field"]]
        fresh(value, c["max_age"])
    elif kind == "file_age":
        fresh(os.stat(c["path"]).st_mtime, c["max_age"])
        with open(c["path"]) as f:
            if c.get("contains", "") not in f.read(65536):
                raise ProbeFailure("content")
    elif kind == "systemd":
        output = command(["systemctl", "show", c["unit"], "-p", "Result", "-p", "ExecMainExitTimestampMonotonic"])
        fields = dict(line.split("=", 1) for line in output.decode().splitlines() if "=" in line)
        if fields.get("Result") != "success":
            raise ProbeFailure("evidence")
        finished = int(fields.get("ExecMainExitTimestampMonotonic", 0)) / 1e6
        if not finished or not 0 <= time.clock_gettime(time.CLOCK_BOOTTIME) - finished <= c["max_age"]:
            raise ProbeFailure("stale")
    elif kind == "certificate":
        with socket.create_connection((c["host"], c.get("port", 443)), timeout=10) as conn:
            with ssl.create_default_context().wrap_socket(conn, server_hostname=c["host"]) as tls:
                expiry = ssl.cert_time_to_seconds(tls.getpeercert()["notAfter"])
                if expiry - time.time() < c.get("days", 21) * 86400:
                    raise ProbeFailure("certificate_expiring")
    elif kind == "certificate_file":
        command(["openssl", "x509", "-in", c["path"], "-noout", "-checkend", str(c.get("days", 21) * 86400)])
    elif kind == "query":
        url = c["url"] + "/api/v1/query?" + urllib.parse.urlencode({"query": c["query"]})
        response = json.loads(request(url))
        if response.get("status") != "success":
            raise ProbeFailure("metric")
        values = response["data"]["result"]
        if len(values) != c.get("count", 1):
            raise ProbeFailure("metric")
        for item in values:
            sample_time, value = item["value"]
            fresh(sample_time, c.get("max_age", 120))
            value = float(value)
            if not math.isfinite(value) or not c.get("min", -math.inf) <= value <= c.get("max", math.inf):
                raise ProbeFailure("metric")
    else:
        raise ValueError("unsupported probe")


def run_one(c, base, dry_run):
    report = None
    try:
        probe(c)
    except ContainerFailure as error:
        report = {"failures": error.failures}
    except Exception as error:
        report = {"error": failure_report(error)}
    if not dry_run:
        try:
            headers = {"Authorization": "Bearer " + c["token"]}
            if report is not None:
                headers["Content-Type"] = "application/json"
            kwargs = {"body": json.dumps(report).encode()} if report is not None else {}
            request(base + "/api/heartbeat/" + urllib.parse.quote(c["id"], safe=""),
                    method="POST", headers=headers, status=204, **kwargs)
        except Exception:
            print(c["id"] + ": delivery failed", flush=True)
            return False
    detail = json.dumps(report, sort_keys=True) if report else "ok"
    print(c["id"] + ": " + detail, flush=True)
    return report is None


def mqtt_probe(c):
    def string(s):
        b = s.encode("utf-8")
        if len(b) > 4096:
            raise ProbeFailure("probe")
        return struct.pack("!H", len(b)) + b

    deadline = time.monotonic() + 10
    with socket.create_connection((c.get("host", "127.0.0.1"), c.get("port", 1883)), timeout=5) as conn:
        def send(kind, body=b""):
            n, length = len(body), b""
            while True:
                length += bytes([(n % 128) | (128 if n >= 128 else 0)])
                n //= 128
                if not n:
                    break
            conn.sendall(bytes([kind]) + length + body)

        def read(n):
            out = b""
            while len(out) < n:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise TimeoutError()
                conn.settimeout(remaining)
                b = conn.recv(n - len(out))
                if not b:
                    raise ConnectionError()
                out += b
            return out

        def packet():
            kind = read(1)[0]
            n, multiplier = 0, 1
            for _ in range(4):
                v = read(1)[0]
                n += (v & 127) * multiplier
                if not v & 128:
                    if n > 65536:
                        raise ProbeFailure("content")
                    return kind, read(n)
                multiplier *= 128
            raise ProbeFailure("content")

        flags = 2
        body = string("monitor-" + uuid.uuid4().hex[:16])
        if "username" in c:
            flags |= 0xc0
            body += string(c["username"]) + string(c["password"])
        send(0x10, string("MQTT") + bytes([4, flags]) + struct.pack("!H", 15) + body)
        kind, data = packet()
        if kind != 0x20 or data != b"\0\0":
            raise ProbeFailure("connection")
        if not c.get("zigbee_base_topic"):
            send(0xc0)
            if packet() != (0xd0, b""):
                raise ProbeFailure("content")
        else:
            base = c["zigbee_base_topic"]
            topic = base + "/bridge/response/health_check"
            send(0x82, b"\0\1" + string(topic) + b"\0")
            kind, data = packet()
            if kind != 0x90 or data != b"\0\1\0":
                raise ProbeFailure("connection")
            transaction = uuid.uuid4().hex
            send(0x30, string(base + "/bridge/request/health_check") + json.dumps({"transaction": transaction}).encode())
            for _ in range(16):
                kind, data = packet()
                if kind >> 4 != 3 or (kind >> 1) & 3 or len(data) < 2:
                    raise ProbeFailure("content")
                length = struct.unpack("!H", data[:2])[0]
                if data[2:2+length].decode() != topic:
                    continue
                value = json.loads(data[2+length:])
                if value.get("transaction") == transaction and not kind & 1:
                    if value.get("status") != "ok" or value.get("data", {}).get("healthy") is not True:
                        raise ProbeFailure("evidence")
                    break
            else:
                raise ProbeFailure("stale")
        send(0xe0)


def export_metrics(config, checks, results):
    host = config["metrics_host"]
    if not re.fullmatch(r"[a-zA-Z0-9_.-]+", host):
        raise ValueError("invalid metric host")
    now = time.time()
    lines = [f'watchlight_collection,host={host} completed_timestamp={now}']
    for check, (ok, duration) in zip(checks, results):
        ident = check["id"]
        if not re.fullmatch(r"[a-zA-Z0-9_-]+", ident):
            raise ValueError("invalid metric identity")
        lines.append(f'watchlight_probe,host={host},check={ident} success={int(ok)}i,duration_seconds={duration}')
    fields = []
    proc = pathlib.Path(config.get("proc_root", "/proc"))
    for resource in ("cpu", "memory", "io"):
        path = proc / "pressure" / resource
        if path.exists():
            for line in path.read_text().splitlines():
                category, *values = line.split()
                if category not in ("some", "full"):
                    continue
                for pair in values:
                    key, value = pair.split("=")
                    if key in ("avg10", "avg60", "avg300", "total"):
                        number = float(value)
                        if math.isfinite(number) and number >= 0:
                            fields.append(f'pressure_{resource}_{category}_{key}={number}')
    for line in (proc / "vmstat").read_text().splitlines():
        key, value = line.split()
        if key == "oom_kill":
            fields.append(f'oom_kills_total={int(value)}i')
    if fields:
        lines.append(f'watchlight_host,host={host} ' + ",".join(fields))
    if config.get("nginx_log"):
        lines.extend(nginx_metrics(config, host))
    path = pathlib.Path(config["metrics_path"])
    temporary = path.with_name(path.name + ".tmp")
    temporary.write_text("\n".join(lines) + "\n")
    temporary.chmod(0o644)
    os.replace(temporary, path)


def nginx_metrics(config, host):
    """Aggregate only a dedicated URL/IP-free JSON log; never export raw lines."""
    path = pathlib.Path(config["nginx_log"])
    state_path = pathlib.Path(config["metrics_path"]).with_name("nginx-counters.json")
    state = json.loads(state_path.read_text()) if state_path.exists() else {"servers": {}}
    stat = path.stat()
    position = state.get("position", 0)
    if state.get("inode") != stat.st_ino or stat.st_size < position:
        position = 0
    with path.open("rb") as stream:
        stream.seek(position)
        stop = position + 4 * 1024 * 1024
        while stream.tell() < stop:
            before = stream.tell()
            line = stream.readline(8192)
            if not line:
                break
            if not line.endswith(b"\n"):
                stream.seek(before)
                break
            try:
                value = json.loads(line)
                server = value["server"]
                if not re.fullmatch(r"[a-zA-Z0-9*_.-]{1,255}", server):
                    continue
                status, size, duration = int(value["status"]), int(value["bytes"]), float(value["duration"])
                if not 100 <= status <= 599 or size < 0 or not math.isfinite(duration) or duration < 0:
                    continue
                if server not in state["servers"] and len(state["servers"]) >= 64:
                    continue
                counts = state["servers"].setdefault(server, {"requests": 0, "errors": 0, "bytes": 0, "duration": 0})
                counts["requests"] += 1
                counts["errors"] += int(status >= 500)
                counts["bytes"] += size
                counts["duration"] += duration
            except (ValueError, KeyError, TypeError):
                continue
        state["position"] = stream.tell()
    state["inode"] = stat.st_ino
    temporary = state_path.with_name(state_path.name + ".tmp")
    temporary.write_text(json.dumps(state))
    temporary.chmod(0o600)
    os.replace(temporary, state_path)
    return [f'nginx_requests,host={host},server={server} requests_total={v["requests"]}i,errors_total={v["errors"]}i,bytes_total={v["bytes"]}i,duration_sum_seconds={v["duration"]}'
            for server, v in state["servers"].items()]


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
        def timed(c):
            start = time.monotonic()
            ok = run_one(c, base.rstrip("/"), dry_run)
            return ok, time.monotonic() - start
        results = list(pool.map(timed, config["checks"]))
    if config.get("metrics_path") and not dry_run:
        try:
            export_metrics(config, config["checks"], results)
        except Exception:
            print("collector: metric export failed", flush=True)
            return 1
    return 0 if all(ok for ok, _ in results) else 1


if __name__ == "__main__":
    sys.exit(main())

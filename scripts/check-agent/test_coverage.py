import importlib.util
import json
import pathlib
import socket
import struct
import tempfile
import threading
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("agent", pathlib.Path(__file__).with_name("agent.py"))
agent = importlib.util.module_from_spec(spec)
spec.loader.exec_module(agent)


class CoverageTests(unittest.TestCase):
    def test_metrics_include_host_oom_and_pressure_without_secrets(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            (root / "pressure").mkdir()
            (root / "pressure/memory").write_text("some avg10=2.5 avg60=1.0 total=100\nfull avg10=0.2 avg60=0.1 total=10\n")
            (root / "vmstat").write_text("oom_kill 3\npgfault 100\n")
            cfg = {"metrics_host": "example", "metrics_path": str(root / "metrics.influx"), "proc_root": directory}
            agent.export_metrics(cfg, [{"id": "sample", "token": "secret"}], [(False, 0.25)])
            text = (root / "metrics.influx").read_text()
            self.assertIn("oom_kills_total=3i", text)
            self.assertIn("pressure_memory_full_avg60=0.1", text)
            self.assertIn("success=0i,duration_seconds=0.25", text)
            self.assertNotIn("secret", text)
            (root / "pressure/memory").unlink()
            agent.export_metrics(cfg, [], [])  # Pressure metrics are optional.

    def test_nginx_aggregation_is_incremental_and_handles_rotation(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            path = root / "requests.jsonl"
            line = json.dumps({"server": "example.com", "status": "503", "bytes": "10", "duration": "0.5"}) + "\n"
            path.write_text(line)
            cfg = {"nginx_log": str(path), "metrics_path": str(root / "metrics.influx")}
            self.assertIn("requests_total=1i,errors_total=1i", agent.nginx_metrics(cfg, "example")[0])
            self.assertIn("requests_total=1i,errors_total=1i", agent.nginx_metrics(cfg, "example")[0])
            path.rename(root / "old.log")
            path.write_text(line)
            self.assertIn("requests_total=2i,errors_total=2i", agent.nginx_metrics(cfg, "example")[0])
            self.assertNotIn("503", (root / "nginx-counters.json").read_text())

    def test_udp_dns_success_and_error(self):
        for flags, answers, ok in [(0x8180, 1, True), (0x8183, 0, False), (0x8380, 1, False)]:
            with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as server:
                server.bind(("127.0.0.1", 0))
                def serve():
                    query, address = server.recvfrom(4096)
                    server.sendto(query[:2] + struct.pack("!HHHHH", flags, 1, answers, 0, 0) + query[12:], address)
                thread = threading.Thread(target=serve)
                thread.start()
                c = {"kind": "dns_udp", "host": "127.0.0.1", "port": server.getsockname()[1], "name": "example.com"}
                if ok:
                    agent.probe(c)
                else:
                    with self.assertRaises(agent.ProbeFailure):
                        agent.probe(c)
                thread.join(2)

    def test_dynamic_container_http_and_local_bearer_file(self):
        with tempfile.TemporaryDirectory() as directory:
            token = pathlib.Path(directory) / "token"
            token.write_text("private-value\n")
            with patch.object(agent, "command", return_value=b"172.22.0.2"), patch.object(agent, "request", return_value=b"ok") as request:
                agent.probe({"kind": "http", "container": "example-api", "network": "example_default", "port": 8080, "path": "/health", "bearer_token_file": str(token)})
                self.assertEqual(request.call_args.args[0], "http://172.22.0.2:8080/health")
                self.assertEqual(request.call_args.args[2], {"Authorization": "Bearer private-value"})

    def test_transport_preserves_status_and_sanitizes_unknown_errors(self):
        for report, code in [({"ok": False, "error": {"code": "http_status", "http_status": 500}}, "http_status"),
                             ({"ok": False, "error": {"code": "private raw error"}}, "probe")]:
            with patch.object(agent, "command", return_value=json.dumps(report).encode()):
                with self.assertRaises(agent.ProbeFailure) as raised:
                    agent.probe({"kind": "transport", "argv": ["example-probe"]})
                self.assertEqual(raised.exception.report["code"], code)

    def test_mqtt_ping_and_fresh_zigbee_response(self):
        for zigbee in (False, True):
            with socket.socket() as server:
                server.bind(("127.0.0.1", 0))
                server.listen(1)
                failures = []
                def serve():
                    try:
                        with server.accept()[0] as conn:
                            def exact(n):
                                b = b""
                                while len(b) < n:
                                    b += conn.recv(n-len(b))
                                return b
                            def packet():
                                kind = exact(1)[0]
                                n, factor = 0, 1
                                while True:
                                    v = exact(1)[0]
                                    n += (v & 127) * factor
                                    if not v & 128:
                                        return kind, exact(n)
                                    factor *= 128
                            self.assertEqual(packet()[0], 0x10)
                            conn.sendall(b"\x20\x02\0\0")
                            if zigbee:
                                self.assertEqual(packet()[0], 0x82)
                                conn.sendall(b"\x90\x03\0\1\0")
                                _, data = packet()
                                n = struct.unpack("!H", data[:2])[0]
                                request = json.loads(data[2+n:])
                                topic = b"example/bridge/response/health_check"
                                body = struct.pack("!H", len(topic)) + topic + json.dumps({"transaction": request["transaction"], "status": "ok", "data": {"healthy": True}}, separators=(",", ":")).encode()
                                length, encoded = len(body), b""
                                while True:
                                    encoded += bytes([(length % 128) | (128 if length >= 128 else 0)])
                                    length //= 128
                                    if not length:
                                        break
                                conn.sendall(b"\x30" + encoded + body)
                            else:
                                self.assertEqual(packet()[0], 0xc0)
                                conn.sendall(b"\xd0\0")
                            self.assertEqual(packet()[0], 0xe0)
                    except Exception as error:
                        failures.append(error)
                thread = threading.Thread(target=serve)
                thread.start()
                c = {"kind": "mqtt", "host": "127.0.0.1", "port": server.getsockname()[1], "username": "example", "password": "private"}
                if zigbee:
                    c["zigbee_base_topic"] = "example"
                agent.probe(c)
                thread.join(2)
                self.assertFalse(failures)


if __name__ == "__main__":
    unittest.main()

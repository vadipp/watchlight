import importlib.util
import pathlib
import tempfile
import time
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("agent", pathlib.Path(__file__).with_name("agent.py"))
agent = importlib.util.module_from_spec(spec)
spec.loader.exec_module(agent)

class AgentTests(unittest.TestCase):
    def test_no_heartbeat_after_failure(self):
        with patch.object(agent, "probe", side_effect=ValueError()), patch.object(agent, "request") as post:
            self.assertFalse(agent.run_one({"id":"fixture","token":"private"},"https://example.com",False))
            post.assert_not_called()
    def test_success_sends_header_token(self):
        with patch.object(agent, "probe"), patch.object(agent, "request") as post:
            self.assertTrue(agent.run_one({"id":"fixture","token":"private"},"https://example.com",False))
            self.assertEqual(post.call_args.kwargs["headers"], {"Authorization":"Bearer private"})
            self.assertNotIn("private",post.call_args.args[0])
    def test_freshness_rejects_old_future_and_failed_jobs(self):
        for value in [time.time()-200,time.time()+200]:
            with self.assertRaises(ValueError): agent.fresh(value,100)
        with tempfile.TemporaryDirectory() as d:
            p=pathlib.Path(d)/"result.json"
            p.write_text('{"success":false,"finished":'+str(time.time())+'}')
            with self.assertRaises(ValueError):
                agent.probe({"kind":"timestamp","path":str(p),"field":"finished","require":{"success":True},"max_age":100})
    def test_missing_or_stale_telemetry_fails(self):
        for result in [[],[{"value":[time.time()-200,"1"]}],[{"value":[time.time(),"NaN"]}]]:
            import json
            with patch.object(agent,"request",return_value=json.dumps({"status":"success","data":{"result":result}}).encode()):
                with self.assertRaises(ValueError):agent.probe({"kind":"query","url":"http://example.com","query":"test","min":0})
    def test_timeout(self):
        import sys
        with self.assertRaises(Exception):agent.command([sys.executable,"-c","import time; time.sleep(3)"],0.05)
    def test_docker_requires_every_container_running_and_healthy(self):
        import json
        healthy = {"Running":True,"Health":{"Status":"healthy"}}
        for state in [{"Running":False},{"Running":True,"Paused":True},{"Running":True,"Restarting":True},{"Running":True,"Health":{"Status":"unhealthy"}},{"Running":True,"Health":{"Status":"starting"}}]:
            output = '\n'.join(json.dumps(s) for s in [healthy,state]).encode()
            with patch.object(agent,"command",return_value=output):
                with self.assertRaises(ValueError):agent.probe({"kind":"docker","containers":["example-api","example-db"]})
        with patch.object(agent,"command",return_value=json.dumps(healthy).encode()):
            with self.assertRaises(ValueError):agent.probe({"kind":"docker","containers":["example-api","missing-worker"]})
        output = '\n'.join(json.dumps(s) for s in [healthy,{"Running":True}]).encode()
        with patch.object(agent,"command",return_value=output):
            agent.probe({"kind":"docker","containers":["example-api","example-worker"]})

class ComposeTests(unittest.TestCase):
    def test_missing_service_and_all_replicas(self):
        import json
        def row(name, service, state):
            return (json.dumps(name)+" "+json.dumps(service)+" "+json.dumps(state)+"\n").encode()
        rows = row("/example-api-1", "api", {"Running":True}) + row("/example-api-2", "api", {"Running":False,"Status":"exited"})
        with patch.object(agent,"command",side_effect=[b"a\nb\n",rows]):
            with self.assertRaises(agent.ContainerFailure) as raised:
                agent.probe({"kind":"compose","project":"example","services":["api","worker"]})
        self.assertEqual(raised.exception.failures,[{"name":"example-api-2","status":"exited"},{"name":"worker","status":"missing"}])
    def test_recreated_container_and_excluded_job(self):
        import json
        rows = b'"/example-api-new" "api" '+json.dumps({"Running":True}).encode()+b'\n"/example-job-1" "job" '+json.dumps({"Running":False}).encode()
        with patch.object(agent,"command",side_effect=[b"a\nb",rows]):
            agent.probe({"kind":"compose","project":"example","services":["api"]})
        with patch.object(agent,"command",return_value=b""):
            with self.assertRaises(agent.ContainerFailure):
                agent.probe({"kind":"compose","project":"example","services":["api"]})
    def test_structured_failure_report_and_dry_run(self):
        import json
        c={"id":"fixture","token":"private"}
        failure=agent.ContainerFailure([{"name":"example-db","status":"unhealthy"}])
        with patch.object(agent,"probe",side_effect=failure),patch.object(agent,"request") as post:
            self.assertFalse(agent.run_one(c,"https://example.com",False))
            self.assertEqual(json.loads(post.call_args.kwargs["body"]),{"failures":failure.failures})
            self.assertNotIn("private",post.call_args.args[0])
        with patch.object(agent,"probe",side_effect=failure),patch.object(agent,"request") as post:
            self.assertFalse(agent.run_one(c,"https://example.com",True));post.assert_not_called()

if __name__ == '__main__': unittest.main()

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

if __name__ == '__main__': unittest.main()

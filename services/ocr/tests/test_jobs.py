"""Queue tests: durable progress, bounded retention, and a full queue that says so."""
from __future__ import annotations

import json
import os
import sys
import tempfile
import threading
import time
import unittest
import urllib.error
import urllib.request
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

HERE = Path(__file__).resolve().parent.parent
SAMPLES = HERE / "samples"

# A short retention window is the point of the test, so configure it before the module loads.
os.environ.setdefault("JOBS_TTL_S", "1")
os.environ["JOBS_DB"] = str(Path(tempfile.mkdtemp()) / "jobs.db")

from server import JOBS, make_server  # noqa: E402


def _post(port: int, path: str, payload: dict) -> tuple[int, dict]:
    req = urllib.request.Request(f"http://127.0.0.1:{port}{path}",
                                 data=json.dumps(payload).encode(),
                                 headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return r.status, json.loads(r.read().decode())
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read().decode())


def _get(port: int, path: str) -> tuple[int, dict]:
    try:
        with urllib.request.urlopen(f"http://127.0.0.1:{port}{path}", timeout=30) as r:
            return r.status, json.loads(r.read().decode())
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read().decode())


class JobQueueTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        SAMPLES.mkdir(exist_ok=True)
        if not (SAMPLES / "text.pdf").exists():
            import subprocess
            subprocess.run([sys.executable, str(SAMPLES / "make_samples.py")], check=True)
        cls.srv = make_server(port=0)
        cls.port = cls.srv.server_address[1]
        threading.Thread(target=cls.srv.serve_forever, daemon=True).start()

    @classmethod
    def tearDownClass(cls) -> None:
        cls.srv.shutdown()
        cls.srv.server_close()

    def _wait_done(self, job_id: str, timeout: float = 20.0) -> dict:
        deadline = time.time() + timeout
        while time.time() < deadline:
            code, body = _get(self.port, f"/v1/jobs/{job_id}")
            self.assertEqual(code, 200)
            if body["status"] in ("done", "failed", "expired"):
                return body
            time.sleep(0.1)
        self.fail(f"job {job_id} did not finish in {timeout}s")

    def test_submit_returns_202_and_a_poll_url(self):
        code, body = _post(self.port, "/v1/jobs", {"path": "samples/text.pdf"})
        self.assertEqual(code, 202)
        self.assertEqual(body["status"], "queued")
        self.assertTrue(body["poll"].endswith(body["id"]))

    def test_job_finishes_with_extracted_text(self):
        code, body = _post(self.port, "/v1/jobs", {"path": "samples/text.pdf"})
        self.assertEqual(code, 202)
        done = self._wait_done(body["id"])
        self.assertEqual(done["status"], "done")
        self.assertIn("ACME Demo Ltd", done["result"]["text"])
        self.assertGreater(done["result"]["chars"], 50)

    def test_bad_path_is_rejected_before_queueing(self):
        code, body = _post(self.port, "/v1/jobs", {"path": "../server.py"})
        self.assertEqual(code, 403)
        self.assertEqual(body["error"], "path_outside_root")
        code, body = _post(self.port, "/v1/jobs", {"path": "samples/nope.pdf"})
        self.assertEqual(code, 404)

    def test_text_expires_but_the_job_row_survives(self):
        code, body = _post(self.port, "/v1/jobs", {"path": "samples/text.pdf"})
        job_id = body["id"]
        done = self._wait_done(job_id)
        self.assertEqual(done["status"], "done")
        time.sleep(1.4)                                  # JOB TTL is 1s in this test
        code, after = _get(self.port, f"/v1/jobs/{job_id}")
        self.assertEqual(code, 200)
        self.assertEqual(after["status"], "expired")
        self.assertNotIn("result", after)
        self.assertIn("removed after its retention window", after["note"])

    def test_purge_removes_orphaned_text(self):
        code, body = _post(self.port, "/v1/jobs", {"path": "samples/text.pdf"})
        self._wait_done(body["id"])
        time.sleep(1.4)
        self.assertGreaterEqual(JOBS.purge(), 1)

    def test_full_queue_is_answered_not_dropped(self):
        """A full queue must be a visible rejection, not a request that hangs or vanishes.
        Tested at the store with no workers, so the queue really stays full."""
        from jobs import JobStore
        store = JobStore(Path(tempfile.mkdtemp()) / "q.db", workers=0, maxsize=2)
        self.assertEqual(store.submit("samples/text.pdf", "eng")["status"], "queued")
        self.assertEqual(store.submit("samples/text.pdf", "eng")["status"], "queued")
        third = store.submit("samples/text.pdf", "eng")
        self.assertEqual(third["status"], "rejected")
        self.assertEqual(third["error"], "queue_full")
        self.assertEqual(store.get(third["id"])["status"], "rejected")

    def test_rejected_job_is_503_over_http(self):
        real = JOBS.submit
        JOBS.submit = lambda path, lang: {"id": "x", "status": "rejected", "error": "queue_full"}
        try:
            code, body = _post(self.port, "/v1/jobs", {"path": "samples/text.pdf"})
            self.assertEqual(code, 503)
            self.assertEqual(body["error"], "queue_full")
        finally:
            JOBS.submit = real

    def test_unknown_job_is_404(self):
        code, _ = _get(self.port, "/v1/jobs/doesnotexist")
        self.assertEqual(code, 404)


if __name__ == "__main__":
    unittest.main(verbosity=2)

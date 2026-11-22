#!/usr/bin/env python3
import copy
import importlib.util
import os
from pathlib import Path
import sys
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("verify", Path(__file__).with_name("verify.py"))
pf = importlib.util.module_from_spec(spec)
spec.loader.exec_module(pf)

FAKE_HELPER = r'''#!{python}
import json
import os
import struct
import sys
import time
mode = os.path.basename(sys.argv[1])
if mode == "crash":
    sys.stderr.write("synthetic shader failure\n")
    sys.exit(9)
if mode == "hang":
    time.sleep(30)
    sys.exit(0)
print(json.dumps({{"device": "cpu", "gpu": False, "ms": 1}}), flush=True)
if mode == "reply-hang":
    time.sleep(30)
    sys.exit(0)
def exact(n):
    b = sys.stdin.buffer.read(n)
    if len(b) != n:
        raise EOFError()
    return b
try:
    while True:
        n = struct.unpack("<I", exact(4))[0]
        texts = [exact(struct.unpack("<I", exact(4))[0]) for _ in range(n)]
        if mode == "short":
            sys.stdout.buffer.write(b"\x00\x00")
            sys.stdout.buffer.flush()
            sys.exit(0)
        if mode == "error":
            message = b"synthetic classification failure"
            sys.stdout.buffer.write(struct.pack("<II", 1, len(message)) + message)
            sys.stdout.buffer.flush()
            continue
        out = bytearray(struct.pack("<I", 0))
        for text in texts:
            label = b"private_person"
            out.extend(struct.pack("<I", 1))
            out.extend(struct.pack("<iifB", 0, len(text), 0.99, len(label)))
            out.extend(label)
        # Send a byte at a time to exercise short reads.
        for byte in out:
            sys.stdout.buffer.write(bytes([byte]))
            sys.stdout.buffer.flush()
except EOFError:
    pass
'''


def record():
    return {"startup": {"device": "cpu", "gpu": False},
            "requested_device": "cpu", "texts": ["张伟"],
            "runs": [{"results": [[{"start": 0, "end": 6, "label": "private_person",
                                     "score": 0.99, "value": "张伟"}]]}]}


class ComparisonTests(unittest.TestCase):
    def test_identical_runs_pass(self):
        self.assertEqual(pf.compare_records(record(), record()), [])

    def test_mismatch_does_not_pass(self):
        candidate = record()
        candidate["runs"][0]["results"][0][0]["label"] = "secret"
        self.assertTrue(pf.compare_records(record(), candidate))

    def test_scores_are_compared(self):
        candidate = record()
        candidate["runs"][0]["results"][0][0]["score"] = 0.55
        self.assertTrue(pf.compare_records(record(), candidate))

    def test_nonfinite_score_is_not_a_pass(self):
        candidate = record()
        candidate["runs"][0]["results"][0][0]["score"] = float("nan")
        self.assertTrue(pf.compare_records(record(), candidate))

    def test_repeated_runs_must_be_stable(self):
        candidate = record()
        changed = copy.deepcopy(candidate["runs"][0])
        changed["results"][0] = []
        candidate["runs"].append(changed)
        self.assertTrue(pf.compare_records(record(), candidate))

    def test_gpu_cpu_fallback_is_rejected(self):
        candidate = record()
        candidate["requested_device"] = "gpu"
        self.assertTrue(pf.compare_records(record(), candidate))

    def test_environment_is_explicit(self):
        env = pf.helper_environment("gpu", {"PF_NOFLASH": "1"})
        self.assertEqual(env["PF_DEVICE"], "gpu")
        self.assertEqual(env["PF_NOFLASH"], "1")


@unittest.skipUnless(os.name == "posix", "helper pipe checks use POSIX select")
class ProtocolTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.binary = self.root / "helper"
        self.binary.write_text(FAKE_HELPER.format(python=sys.executable))
        self.binary.chmod(0o755)

    def tearDown(self):
        self.temp.cleanup()

    def run_mode(self, mode, device="cpu", timeout=10):
        return pf.run_helper(self.binary, mode, device, ["张伟"], 2,
                             self.root / "stderr.log", timeout, timeout)

    def test_u8_label_and_unicode_byte_offsets(self):
        result = self.run_mode("ok")
        self.assertNotIn("error", result)
        self.assertEqual(len(result["runs"]), 2)
        span = result["runs"][0]["results"][0][0]
        self.assertEqual((span["end"], span["value"]), (6, "张伟"))
        self.assertEqual(span["label"], "private_person")

    def test_startup_crash_preserves_stderr_and_exit_code(self):
        result = self.run_mode("crash")
        self.assertIn("error", result)
        self.assertIn("synthetic shader failure", result["stderr_tail"])
        self.assertEqual(result["returncode"], 9)

    def test_startup_timeout_terminates_helper(self):
        result = self.run_mode("hang", timeout=0.2)
        self.assertIn("TimeoutError", result["error"])
        self.assertIsNotNone(result["returncode"])

    def test_reply_timeout_terminates_helper(self):
        result = self.run_mode("reply-hang", timeout=0.2)
        self.assertIn("TimeoutError", result["error"])

    def test_short_reply_fails(self):
        result = self.run_mode("short")
        self.assertIn("ProtocolError", result["error"])
        self.assertEqual(result["runs"], [])

    def test_error_reply_fails(self):
        result = self.run_mode("error")
        self.assertIn("synthetic classification failure", result["error"])

    def test_cpu_fallback_fails_gpu_run(self):
        result = self.run_mode("ok", device="gpu")
        self.assertIn("fell back to CPU", result["error"])
        self.assertEqual(result["runs"], [])


if __name__ == "__main__":
    unittest.main()

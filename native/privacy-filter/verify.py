#!/usr/bin/env python3
"""Exercise pf-serve and compare the same inputs on CPU and GPU.

python3 -I verify.py run BINARY MODEL --device cpu --output cpu.json
python3 -I verify.py run BINARY MODEL --device gpu --output gpu.json
python3 -I verify.py compare cpu.json gpu.json
"""
import argparse
import json
import math
import os
from pathlib import Path
import select
import struct
import subprocess
import sys
import time

TEXTS = [
    "Alice Johnson lives at 742 Evergreen Terrace, Springfield. "
    "Email alice.j@example.com, phone +1 (555) 010-2999.",
    "联系 张伟，手机 138-0013-8000，邮箱 zhangwei@example.cn，身份证号见纸质文件。",
    "function updateProfile(user) { return fetch(`/api/users/${user.id}`); } "
    "// token ghp_0123456789abcdefghijklmnopqrstuvwxyz",
    "My SSN is 123-45-6789 and my IBAN is GB82 WEST 1234 5698 7654 32.",
    "Nothing sensitive here, just plain words about the weather and the sea.",
]


class ProtocolError(RuntimeError):
    pass


def read_exact(stream, size, deadline):
    data = bytearray()
    while len(data) < size:
        remaining = deadline - time.monotonic()
        if remaining <= 0 or not select.select([stream], [], [], remaining)[0]:
            raise TimeoutError("timed out reading pf-serve")
        chunk = os.read(stream.fileno(), size - len(data))
        if not chunk:
            raise ProtocolError("pf-serve closed stdout before completing its reply")
        data.extend(chunk)
    return bytes(data)


def read_hello(stream, deadline):
    line = bytearray()
    while len(line) < 65536:
        byte = read_exact(stream, 1, deadline)
        if byte == b"\n":
            try:
                return json.loads(line)
            except (ValueError, UnicodeError) as exc:
                raise ProtocolError("invalid startup JSON") from exc
        line.extend(byte)
    raise ProtocolError("startup JSON exceeds 64 KiB")


def validate_hello(hello, device):
    if not isinstance(hello, dict) or "error" in hello:
        raise ProtocolError(f"model did not start: {hello}")
    actual = hello.get("device")
    if not isinstance(actual, str) or not actual:
        raise ProtocolError("startup reply has no device name")
    if type(hello.get("gpu")) is not bool:
        raise ProtocolError("startup reply has no boolean GPU status")
    if device == "gpu" and (not hello["gpu"] or actual.lower() == "cpu"):
        raise ProtocolError(f"GPU test fell back to CPU: {hello}")
    if device == "cpu" and (hello["gpu"] or actual.lower() != "cpu"):
        raise ProtocolError(f"CPU test used a different device: {hello}")


def encode_request(texts):
    out = bytearray(struct.pack("<I", len(texts)))
    for text in texts:
        raw = text.encode("utf-8")
        out.extend(struct.pack("<I", len(raw)))
        out.extend(raw)
    return bytes(out)


def write_all(stream, data, deadline):
    os.set_blocking(stream.fileno(), False)
    offset = 0
    while offset < len(data):
        remaining = deadline - time.monotonic()
        if remaining <= 0 or not select.select([], [stream], [], remaining)[1]:
            raise TimeoutError("timed out writing to pf-serve")
        try:
            offset += os.write(stream.fileno(), data[offset:offset + 4096])
        except BlockingIOError:
            continue


def read_reply(stream, texts, deadline):
    def u32():
        return struct.unpack("<I", read_exact(stream, 4, deadline))[0]

    status = u32()
    if status == 1:
        length = u32()
        if length > 1024 * 1024:
            raise ProtocolError("error reply exceeds 1 MiB")
        message = read_exact(stream, length, deadline).decode("utf-8", "replace")
        raise ProtocolError(f"classification failed: {message}")
    if status != 0:
        raise ProtocolError(f"unknown reply status {status}")

    results = []
    for text in texts:
        raw = text.encode("utf-8")
        count = u32()
        if count > len(raw) + 1:
            raise ProtocolError("span count exceeds input length")
        spans = []
        for _ in range(count):
            start, end, score = struct.unpack("<iif", read_exact(stream, 12, deadline))
            length = read_exact(stream, 1, deadline)[0]
            label = read_exact(stream, length, deadline).decode("utf-8")
            if not 0 <= start < end <= len(raw):
                raise ProtocolError(f"invalid byte interval [{start}, {end})")
            if not math.isfinite(score) or not 0 <= score <= 1:
                raise ProtocolError(f"invalid confidence score {score}")
            if not label:
                raise ProtocolError("empty entity label")
            # Offsets are bytes, not Python character indices.
            raw[:start].decode("utf-8")
            value = raw[start:end].decode("utf-8")
            spans.append({"start": start, "end": end, "label": label,
                          "score": score, "value": value})
        results.append(spans)
    return results


def helper_environment(device, overrides):
    env = {key: value for key, value in os.environ.items()
           if not key.startswith(("PF_", "GGML_"))}
    env.update(overrides)
    env["PF_DEVICE"] = device
    env["GGML_METAL_NO_RESIDENCY"] = "1"
    return env


def run_helper(binary, model, device, texts, repeats, stderr_path,
               load_timeout=180, request_timeout=120, overrides=None):
    record = {"schema": 1, "binary": str(Path(binary).resolve()),
              "model": str(Path(model).resolve()), "requested_device": device,
              "texts": texts, "runs": [], "stderr": str(stderr_path),
              "environment": dict(overrides or {})}
    proc = None
    start = time.monotonic()
    try:
        with open(stderr_path, "xb") as errlog:
            proc = subprocess.Popen([record["binary"], record["model"]],
                                    stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                    stderr=errlog, bufsize=0,
                                    env=helper_environment(device, overrides or {}))
            try:
                hello = read_hello(proc.stdout, start + load_timeout)
                record["startup"] = hello
                validate_hello(hello, device)
                record["load_elapsed_ms"] = (time.monotonic() - start) * 1000
                payload = encode_request(texts)
                for _ in range(repeats):
                    request_start = time.monotonic()
                    deadline = request_start + request_timeout
                    write_all(proc.stdin, payload, deadline)
                    results = read_reply(proc.stdout, texts, deadline)
                    record["runs"].append({"elapsed_ms": (time.monotonic() - request_start) * 1000,
                                           "results": results})
                proc.stdin.close()
                proc.wait(timeout=10)
                if proc.returncode:
                    raise ProtocolError(f"helper exited with code {proc.returncode}")
            finally:
                if proc.poll() is None:
                    proc.terminate()
                    try:
                        proc.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        proc.kill()
                        proc.wait(timeout=5)
                for stream in (proc.stdin, proc.stdout):
                    if stream and not stream.closed:
                        stream.close()
    except (OSError, ValueError, RuntimeError, TimeoutError, subprocess.TimeoutExpired) as exc:
        record["error"] = f"{type(exc).__name__}: {exc}"
    if proc:
        record["returncode"] = proc.returncode
    if "error" in record and Path(stderr_path).exists():
        with open(stderr_path, "rb") as log:
            log.seek(max(0, os.fstat(log.fileno()).st_size - 4000))
            record["stderr_tail"] = log.read().decode("utf-8", "replace")
    return record


def compare_records(reference, candidate, score_tolerance=0.001):
    errors = []
    for name, record in (("reference", reference), ("candidate", candidate)):
        if record.get("error") or not record.get("runs"):
            errors.append(f"{name} is not a successful run: {record.get('error')}")
        for run in record.get("runs", []):
            for spans in run.get("results", []):
                if any(not math.isfinite(s["score"]) or not 0 <= s["score"] <= 1 for s in spans):
                    errors.append(f"{name} contains an invalid confidence score")
        try:
            validate_hello(record.get("startup"), record.get("requested_device"))
        except ProtocolError as exc:
            errors.append(f"{name}: {exc}")
    if reference.get("texts") != candidate.get("texts"):
        errors.append("inputs differ")
    if errors:
        return errors

    expected = reference["runs"][0]["results"]
    for name, record in (("reference", reference), ("candidate", candidate)):
        for repeat, run in enumerate(record["runs"]):
            actual = run["results"]
            if len(actual) != len(expected):
                errors.append(f"{name} repeat {repeat}: reply text count differs")
                continue
            for index, (left, right) in enumerate(zip(expected, actual)):
                key = lambda span: (span["start"], span["end"], span["label"])
                left = sorted(left, key=key)
                right = sorted(right, key=key)
                if list(map(key, left)) != list(map(key, right)):
                    errors.append(f"{name} repeat {repeat}, text {index}: entity intervals/labels differ")
                    continue
                for a, b in zip(left, right):
                    if abs(a["score"] - b["score"]) > score_tolerance:
                        errors.append(f"{name} repeat {repeat}, text {index}: score difference "
                                      f"{abs(a['score'] - b['score']):.6g} exceeds {score_tolerance}")
    return errors


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    run = sub.add_parser("run")
    run.add_argument("binary")
    run.add_argument("model")
    run.add_argument("--device", choices=("cpu", "gpu"), required=True)
    run.add_argument("--output", type=Path, required=True)
    run.add_argument("--texts", type=Path)
    run.add_argument("--repeats", type=int, default=3)
    run.add_argument("--load-timeout", type=float, default=180)
    run.add_argument("--request-timeout", type=float, default=120)
    run.add_argument("--env", action="append", default=[], metavar="KEY=VALUE")
    compare = sub.add_parser("compare")
    compare.add_argument("reference", type=Path)
    compare.add_argument("candidate", type=Path)
    compare.add_argument("--score-tolerance", type=float, default=0.001)
    args = parser.parse_args()
    if args.command == "compare":
        with args.reference.open() as stream:
            reference = json.load(stream)
        with args.candidate.open() as stream:
            candidate = json.load(stream)
        errors = compare_records(reference, candidate, args.score_tolerance)
        print("FAIL" if errors else "PASS")
        for error in errors:
            print(error)
        return 1 if errors else 0

    if args.repeats < 1 or args.load_timeout <= 0 or args.request_timeout <= 0:
        parser.error("repeat count and timeouts must be positive")
    overrides = {}
    for item in args.env:
        key, sep, value = item.partition("=")
        if not sep or not key.startswith(("PF_", "GGML_")) or key == "PF_DEVICE":
            parser.error("--env needs a PF_/GGML_ key other than PF_DEVICE")
        overrides[key] = value
    texts = TEXTS
    if args.texts:
        with args.texts.open() as stream:
            texts = json.load(stream)
        if not isinstance(texts, list) or not all(isinstance(t, str) for t in texts):
            parser.error("--texts must contain a JSON array of strings")
    if args.output.exists():
        parser.error("output exists; choose a new path to preserve earlier evidence")
    record = run_helper(args.binary, args.model, args.device, texts, args.repeats,
                        args.output.with_suffix(".stderr.log"),
                        args.load_timeout, args.request_timeout, overrides)
    with args.output.open("x") as stream:
        json.dump(record, stream, ensure_ascii=False, indent=2, allow_nan=False)
        stream.write("\n")
    print(json.dumps({"output": str(args.output), "startup": record.get("startup"),
                      "runs": len(record["runs"]), "error": record.get("error")}, ensure_ascii=False))
    return 1 if record.get("error") else 0


if __name__ == "__main__":
    sys.exit(main())

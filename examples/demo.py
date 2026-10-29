#!/usr/bin/env python3
"""Dependency-free upload/poll/download verification and measured benchmark."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import struct
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import zlib


def fixture():
    width, height = 640, 360
    raw = b"".join(
        b"\0" + bytes(v for x in range(width)
                       for v in (x % 256, y % 256, (x + y) % 256))
        for y in range(height)
    )

    def chunk(kind, data):
        return (struct.pack(">I", len(data)) + kind + data
                + struct.pack(">I", zlib.crc32(kind + data)))

    return (b"\x89PNG\r\n\x1a\n"
            + chunk(b"IHDR", struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0))
            + chunk(b"IDAT", zlib.compress(raw)) + chunk(b"IEND", b""))


def dimensions(data, fmt):
    if fmt == "png" and data.startswith(b"\x89PNG\r\n\x1a\n"):
        return struct.unpack(">II", data[16:24])
    if fmt == "jpeg" and data.startswith(b"\xff\xd8"):
        pos = 2
        while pos + 4 <= len(data):
            if data[pos] != 255:
                raise ValueError("invalid JPEG marker")
            while data[pos] == 255:
                pos += 1
            marker = data[pos]
            pos += 1
            if marker == 0xD9 or marker == 0xDA:
                break
            if marker in range(0xD0, 0xD8) or marker == 1:
                continue
            length = int.from_bytes(data[pos:pos + 2], "big")
            if marker in (0xC0, 0xC1, 0xC2):
                height, width = struct.unpack(">HH", data[pos + 3:pos + 7])
                return width, height
            if length < 2:
                break
            pos += length
    if fmt == "webp" and data[:4] == b"RIFF" and data[8:12] == b"WEBP":
        kind, payload = data[12:16], data[20:]
        if kind == b"VP8X":
            return (int.from_bytes(payload[4:7], "little") + 1,
                    int.from_bytes(payload[7:10], "little") + 1)
        if kind == b"VP8 " and payload[3:6] == b"\x9d\x01\x2a":
            return tuple(v & 0x3FFF for v in struct.unpack("<HH", payload[6:10]))
        if kind == b"VP8L" and payload[0] == 0x2F:
            bits = int.from_bytes(payload[1:5], "little")
            return (bits & 0x3FFF) + 1, ((bits >> 14) & 0x3FFF) + 1
    if fmt == "avif" and data[4:8] == b"ftyp" and b"avif" in data[8:32]:
        # libheif stores primary dimensions in the ispe full box.
        pos = data.find(b"ispe")
        if pos >= 4 and len(data) >= pos + 16:
            return struct.unpack(">II", data[pos + 8:pos + 16])
    raise ValueError("unrecognized format or dimension header: " + fmt)


class Client:
    def __init__(self, base, key, deadline):
        self.base = base.rstrip("/") + "/"
        self.key = key
        self.deadline = deadline
        self.origin = urllib.parse.urlsplit(self.base)[:2]
        # Never forward credentials through an HTTP redirect.
        class NoRedirect(urllib.request.HTTPRedirectHandler):
            def redirect_request(self, *args, **kwargs):
                return None
        self.opener = urllib.request.build_opener(NoRedirect)

    def request(self, path, body=None, content_type=None, method=None):
        url = urllib.parse.urljoin(self.base, path)
        if urllib.parse.urlsplit(url)[:2] != self.origin:
            raise ValueError("refusing to send credentials to another origin")
        remaining = self.deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError("demo deadline exceeded")
        headers = {"Authorization": "Bearer " + self.key}
        if content_type:
            headers["Content-Type"] = content_type
        request = urllib.request.Request(url, body, headers, method=method)
        with self.opener.open(request, timeout=min(remaining, 30)) as response:
            # Match the documented aggregate output ceiling; reject unbounded reads.
            data = response.read(209715201)
            if len(data) > 209715200:
                raise ValueError("response exceeds demo download ceiling")
            return data

    def json(self, path, *args):
        return json.loads(self.request(path, *args))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("sample", nargs="?", help="JPEG/PNG; default generated PNG")
    parser.add_argument("--url", default=os.environ.get("BYTEDOWN_URL", "http://127.0.0.1:8080"))
    parser.add_argument("--profile", default="web")
    parser.add_argument("--timeout", type=float, default=240)
    parser.add_argument("--output", default="demo-output", help="relative output directory")
    parser.add_argument("--benchmark", action="store_true", help="report measured client/platform/version data")
    parser.add_argument("--runtime-info", help="text file with server probe/native versions for the benchmark")
    parser.add_argument("--delete", action="store_true", help="delete uploaded image after verification")
    args = parser.parse_args()
    key = os.environ.get("API_KEY", "")
    if len(key) < 32 or args.timeout <= 0:
        parser.error("set API_KEY (32+ characters) and a positive timeout")
    output = Path(args.output)
    if output.is_absolute() or ".." in output.parts:
        parser.error("--output must be a relative path without '..'")
    data = Path(args.sample).read_bytes() if args.sample else fixture()
    if len(data) > 20971520:
        parser.error("sample exceeds demo's 20 MiB ceiling")
    fmt = "png" if data.startswith(b"\x89PNG") else "jpeg"
    dimensions(data, fmt)
    output.mkdir(parents=True, exist_ok=True)
    started = time.monotonic()
    client = Client(args.url, key, started + args.timeout)
    capabilities = client.json("/v1/capabilities")
    boundary = "bytedown-" + os.urandom(16).hex()
    body = (f"--{boundary}\r\nContent-Disposition: form-data; name=\"profile\"\r\n\r\n"
            f"{args.profile}\r\n--{boundary}\r\nContent-Disposition: form-data; "
            f"name=\"file\"; filename=\"sample.{fmt}\"\r\n"
            f"Content-Type: image/{fmt}\r\n\r\n").encode() + data
    body += f"\r\n--{boundary}--\r\n".encode()
    accepted = client.json("/v1/images", body, "multipart/form-data; boundary=" + boundary)
    upload_ms = (time.monotonic() - started) * 1000
    while True:
        job = client.json(accepted["status_url"])
        if job["status"] in ("succeeded", "failed"):
            break
        if time.monotonic() + 0.5 >= client.deadline:
            raise TimeoutError("job did not finish within deadline")
        time.sleep(0.5)
    if job["status"] != "succeeded":
        raise RuntimeError("job failed: " + str(job.get("error_code", "unknown")))
    completed_ms = (time.monotonic() - started) * 1000
    image = client.json("/v1/images/" + accepted["image_id"])
    original = client.request(image["original_url"])
    if original != data or len(original) != image["bytes"]:
        raise ValueError("original bytes changed")
    (output / ("original." + fmt)).write_bytes(original)
    if len(image["outputs"]) != job["planned"] or job["generated"] != job["planned"]:
        raise ValueError("incomplete manifest")
    for i, variant in enumerate(image["outputs"]):
        result = client.request(variant["url"])
        if len(result) != variant["bytes"]:
            raise ValueError("variant byte count mismatch")
        if dimensions(result, variant["format"]) != (variant["width"], variant["height"]):
            raise ValueError("variant magic/dimensions mismatch")
        (output / f'{i:02d}.{variant["format"]}').write_bytes(result)
    (output / "manifest.json").write_text(json.dumps(image, indent=2) + "\n")
    report = {
        "input_bytes": len(data), "input_sha256": hashlib.sha256(data).hexdigest(),
        "outputs": image["outputs"], "upload_and_capabilities_ms": round(upload_ms, 2),
        "upload_to_job_complete_ms": round(completed_ms, 2),
        "total_including_download_verification_ms": round((time.monotonic() - started) * 1000, 2),
        "server_processing_ms": job.get("processing_ms"),
        "note": "One real run; polling/network overhead included; no quality or compression guarantee.",
    }
    if args.benchmark:
        report.update(client_platform=platform.platform(), python=sys.version,
                      capabilities=capabilities,
                      server_runtime_info=(Path(args.runtime_info).read_text()[:65536]
                                           if args.runtime_info else "not supplied"),
                      deployment_notes=os.environ.get("BENCHMARK_ENV", "not supplied"))
    (output / "report.json").write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps(report, indent=2))
    if args.delete:
        client.request("/v1/images/" + accepted["image_id"], method="DELETE")


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, OSError, TimeoutError, RuntimeError) as error:
        # HTTP errors expose only status, not response headers or credentials.
        print("demo failed: " + (f"HTTP {error.code}" if isinstance(error, urllib.error.HTTPError)
                                else str(error)), file=sys.stderr)
        sys.exit(1)

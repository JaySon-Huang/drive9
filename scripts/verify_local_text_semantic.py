#!/usr/bin/env python3

"""Verify local durable generate_file_semantic_text flows against drive9-server-local.

Exercises these local-validation paths:

- Direct PUT to `/v1/fs/<path>`
- Overwrite via a second PUT to the same path
- Multipart v1: `/v1/uploads/initiate` -> part PUTs -> `/complete`
- Multipart v2: `/v2/uploads/initiate` -> `presign-batch` -> part PUTs -> `/complete`
- Negative small-file path: upload a direct-text file below the large-file threshold
  and assert no durable `generate_file_semantic_text` task is created

Requires TiDB auto-embedding plus `DRIVE9_TEXT_SEMANTIC_ENABLED=true`.

- `--mode basic` (default): rely on the built-in deterministic
  BasicTextSemanticGenerator and assert stable structure/content.
- `--mode openai`: allow provider-backed generation and only assert
  non-empty semantic text plus durable task completion.

The script exits non-zero on any failed assertion.
"""

from __future__ import annotations

import argparse
import base64
import json
import pathlib
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from dataclasses import dataclass
from typing import Any


DEFAULT_BASE_URL = "http://127.0.0.1:9009"
PART_SIZE = 8 * 1024 * 1024
SMALL_FILE_THRESHOLD = 50_000


def sql_string_literal(value: str) -> str:
    """Escape a value for use inside a single-quoted SQL literal."""
    return value.replace("'", "''")


def crc32c_castagnoli(data: bytes) -> int:
    """CRC32C for v1 multipart part_checksums (4-byte digest, base64 on wire)."""
    crc = 0xFFFFFFFF
    for b in data:
        crc ^= b
        for _ in range(8):
            crc = (crc >> 1) ^ (0x82F63B78 if crc & 1 else 0)
    return (~crc) & 0xFFFFFFFF


@dataclass
class VerificationResult:
    flow: str
    path: str
    file_id: str
    revision: int
    task_id: str
    task_type: str
    status: str
    attempt_count: int
    content_text: str


def build_large_text_payload(tag: str) -> bytes:
    """Build a stable large direct-text payload that exceeds the durable-task threshold."""
    header_lines = [
        f"package semanticprobe_{tag}",
        "",
        f"type SemanticProbe{tag.title()} struct {{",
        "    ID string",
        "    Summary string",
        "}",
        "",
        f"func HandleSemanticProbe{tag.title()}(input string) string {{",
        '    return "semantic probe " + input',
        "}",
        "",
        f"const SemanticProbeKeyword{tag.title()} = \"semantic-probe-{tag}\"",
        "",
    ]
    filler = (
        f"section semantic_probe_{tag} explains retrieval oriented indexing, "
        f"large direct text processing, semantic closure, and content text synthesis.\n"
    )
    payload = "\n".join(header_lines) + filler * 900
    data = payload.encode("utf-8")
    if len(data) <= SMALL_FILE_THRESHOLD:
        raise RuntimeError(f"large text payload too small: {len(data)} bytes")
    return data


def build_small_text_payload() -> bytes:
    return b"hello small semantic world\n"


def read_large_text_file_payload(text_file: str) -> bytes:
    path = pathlib.Path(text_file)
    data = path.read_bytes()
    print(
        json.dumps(
            {
                "text_file": str(path),
                "bytes": len(data),
                "small_file_threshold": SMALL_FILE_THRESHOLD,
            },
            ensure_ascii=False,
        )
    )
    if len(data) <= SMALL_FILE_THRESHOLD:
        raise RuntimeError(
            f"text_file {path} is {len(data)} bytes, must be > {SMALL_FILE_THRESHOLD} "
            "to trigger large-file semantic extraction"
        )
    return data


def normalize_remote_path(remote_path: str) -> str:
    remote_path = remote_path.strip()
    if not remote_path:
        raise ValueError("remote path must not be empty")
    if not remote_path.startswith("/"):
        remote_path = "/" + remote_path
    return remote_path


def make_unique_path(prefix: str, ext: str = ".txt") -> str:
    suffix = uuid.uuid4().hex[:10]
    if not ext.startswith("."):
        ext = "." + ext
    return normalize_remote_path(f"/text-semantic/{prefix}-{suffix}{ext}")


def print_result(result: VerificationResult) -> None:
    print(
        json.dumps(
            {
                "flow": result.flow,
                "path": result.path,
                "file_id": result.file_id,
                "revision": result.revision,
                "task_id": result.task_id,
                "task_type": result.task_type,
                "status": result.status,
                "attempt_count": result.attempt_count,
                "content_text": result.content_text,
            },
            ensure_ascii=False,
        )
    )


class Verifier:
    def __init__(
        self, base_url: str, timeout_seconds: float, poll_interval_seconds: float
    ) -> None:
        self.base_url = base_url.rstrip("/")
        self.timeout_seconds = timeout_seconds
        self.poll_interval_seconds = poll_interval_seconds

    def request_json(
        self,
        method: str,
        path: str,
        payload: bytes | None = None,
        headers: dict[str, str] | None = None,
        timeout: float | None = None,
    ) -> Any:
        req = urllib.request.Request(
            self.base_url + path,
            data=payload,
            method=method,
            headers=headers or {},
        )
        with urllib.request.urlopen(
            req, timeout=timeout or self.timeout_seconds
        ) as resp:
            body = resp.read()
            if not body:
                return None
            return json.loads(body.decode())

    def request_status(
        self,
        method: str,
        path: str,
        payload: bytes | None = None,
        headers: dict[str, str] | None = None,
        timeout: float | None = None,
    ) -> tuple[int, bytes]:
        req = urllib.request.Request(
            self.base_url + path,
            data=payload,
            method=method,
            headers=headers or {},
        )
        try:
            with urllib.request.urlopen(
                req, timeout=timeout or self.timeout_seconds
            ) as resp:
                return resp.status, resp.read()
        except urllib.error.HTTPError as exc:
            return exc.code, exc.read()

    def exec_sql(self, query: str) -> list[dict[str, Any]]:
        payload = json.dumps({"query": query}).encode()
        result = self.request_json(
            "POST",
            "/v1/sql",
            payload,
            headers={"Content-Type": "application/json"},
        )
        if not isinstance(result, list):
            raise RuntimeError(f"unexpected SQL result payload: {result!r}")
        return result

    def wait_for_text_semantic_success(self, path: str) -> VerificationResult:
        path_lit = sql_string_literal(path)
        query = (
            "SELECT n.path, f.file_id, f.revision, "
            "COALESCE(f.content_text, '') AS content_text, "
            "t.task_id, t.task_type, t.status, t.attempt_count, "
            "COALESCE(t.last_error, '') AS last_error "
            "FROM file_nodes n "
            "JOIN files f ON f.file_id = n.file_id "
            "LEFT JOIN semantic_tasks t "
            "  ON t.resource_id = f.file_id AND t.resource_version = f.revision "
            f"WHERE n.path = '{path_lit}'"
        )
        deadline = time.time() + self.timeout_seconds
        last_rows: list[dict[str, Any]] = []
        while time.time() < deadline:
            last_rows = self.exec_sql(query)
            if last_rows:
                row = last_rows[0]
                if (
                    row.get("task_type") == "generate_file_semantic_text"
                    and row.get("status") == "succeeded"
                    and row.get("content_text")
                ):
                    return VerificationResult(
                        flow="unknown",
                        path=row["path"],
                        file_id=row["file_id"],
                        revision=int(row["revision"]),
                        task_id=row["task_id"],
                        task_type=row["task_type"],
                        status=row["status"],
                        attempt_count=int(row["attempt_count"]),
                        content_text=row["content_text"],
                    )
            time.sleep(self.poll_interval_seconds)
        raise RuntimeError(
            "timed out waiting for durable generate_file_semantic_text success for "
            f"{path}; last rows: {json.dumps(last_rows, ensure_ascii=False)}"
        )

    def wait_for_small_direct_text(self, path: str) -> str:
        path_lit = sql_string_literal(path)
        query = (
            "SELECT COALESCE(f.content_text, '') AS content_text "
            "FROM file_nodes n "
            "JOIN files f ON f.file_id = n.file_id "
            f"WHERE n.path = '{path_lit}'"
        )
        deadline = time.time() + self.timeout_seconds
        last_rows: list[dict[str, Any]] = []
        while time.time() < deadline:
            last_rows = self.exec_sql(query)
            if last_rows and last_rows[0].get("content_text") is not None:
                return str(last_rows[0]["content_text"])
            time.sleep(self.poll_interval_seconds)
        raise RuntimeError(
            f"timed out waiting for content_text on small file {path}; last rows: {json.dumps(last_rows, ensure_ascii=False)}"
        )

    def assert_no_text_semantic_task(self, path: str, settle_seconds: float) -> None:
        path_lit = sql_string_literal(path)
        query = (
            "SELECT n.path, f.revision, t.task_id, t.task_type, t.status, "
            "COALESCE(f.content_text, '') AS content_text "
            "FROM file_nodes n "
            "JOIN files f ON f.file_id = n.file_id "
            "LEFT JOIN semantic_tasks t "
            "  ON t.resource_id = f.file_id AND t.resource_version = f.revision "
            f"WHERE n.path = '{path_lit}'"
        )
        time.sleep(settle_seconds)
        rows = self.exec_sql(query)
        if not rows:
            raise RuntimeError(f"missing row for {path} after upload")
        row = rows[0]
        if row.get("task_type") == "generate_file_semantic_text":
            raise RuntimeError(
                f"unexpected generate_file_semantic_text task for {path}: "
                f"{json.dumps(rows, ensure_ascii=False)}"
            )

    def calc_part_checksums(self, payload: bytes) -> list[str]:
        checksums = []
        for start in range(0, len(payload), PART_SIZE):
            chunk = payload[start : start + PART_SIZE]
            digest = crc32c_castagnoli(chunk).to_bytes(4, byteorder="big")
            checksums.append(base64.b64encode(digest).decode())
        return checksums

    def upload_parts_from_plan(self, plan: dict[str, Any], payload: bytes) -> None:
        for part in plan["parts"]:
            number = int(part["number"])
            start = (number - 1) * int(plan["part_size"])
            chunk = payload[start : start + int(part["size"])]
            headers = {k: str(v) for k, v in (part.get("headers") or {}).items()}
            headers["Content-Length"] = str(len(chunk))
            req = urllib.request.Request(
                part["url"], data=chunk, method="PUT", headers=headers
            )
            with urllib.request.urlopen(
                req, timeout=max(self.timeout_seconds, 60)
            ) as resp:
                if resp.status != 200:
                    raise RuntimeError(
                        f"multipart part {number} upload failed with status {resp.status}"
                    )

    def complete_upload_v1(self, upload_id: str, path: str) -> None:
        status, body = self.request_status(
            "POST",
            f"/v1/uploads/{upload_id}/complete",
            payload=b"",
        )
        if status != 200:
            raise RuntimeError(
                f"v1 multipart complete failed for {path}: status={status}, body={body.decode(errors='replace')}"
            )

    def put_s3_part(self, part_url: str, chunk: bytes, headers: dict[str, str]) -> str:
        request_headers = dict(headers)
        request_headers["Content-Length"] = str(len(chunk))
        req = urllib.request.Request(
            part_url, data=chunk, method="PUT", headers=request_headers
        )
        with urllib.request.urlopen(
            req, timeout=max(self.timeout_seconds, 120)
        ) as resp:
            if resp.status != 200:
                raise RuntimeError(f"S3 part PUT failed: HTTP {resp.status}")
            etag = resp.headers.get("ETag") or resp.headers.get("etag") or ""
            return etag.strip('"')

    def put_file_best_effort(self, path: str, payload: bytes) -> None:
        checksums = self.calc_part_checksums(payload)
        status, body = self.request_status(
            "PUT",
            "/v1/fs" + path,
            payload=payload,
            headers={
                "Content-Length": str(len(payload)),
                "X-Dat9-Part-Checksums": ",".join(checksums),
            },
        )
        if status not in (200, 202):
            raise RuntimeError(
                f"PUT failed for {path}: status={status}, body={body.decode(errors='replace')}"
            )
        if status == 202:
            plan = json.loads(body.decode())
            self.upload_parts_from_plan(plan, payload)
            self.complete_upload_v1(str(plan["upload_id"]), path)

    def verify_direct_put_bytes(self, path: str, payload: bytes) -> VerificationResult:
        self.put_file_best_effort(path, payload)
        result = self.wait_for_text_semantic_success(path)
        result.flow = "direct_put"
        return result

    def verify_multipart_v1(self, path: str, payload: bytes) -> VerificationResult:
        checksums = self.calc_part_checksums(payload)
        initiate_payload = json.dumps(
            {
                "path": path,
                "total_size": len(payload),
                "part_checksums": checksums,
            }
        ).encode()
        plan = self.request_json(
            "POST",
            "/v1/uploads/initiate",
            payload=initiate_payload,
            headers={"Content-Type": "application/json"},
        )
        if (
            not isinstance(plan, dict)
            or not plan.get("upload_id")
            or not plan.get("parts")
        ):
            raise RuntimeError(f"unexpected v1 multipart initiate payload: {plan!r}")
        self.upload_parts_from_plan(plan, payload)
        self.complete_upload_v1(str(plan["upload_id"]), path)
        result = self.wait_for_text_semantic_success(path)
        result.flow = "multipart_v1"
        return result

    def verify_multipart_v2(self, path: str, payload: bytes) -> VerificationResult:
        initiate_payload = json.dumps({"path": path, "total_size": len(payload)}).encode()
        plan = self.request_json(
            "POST",
            "/v2/uploads/initiate",
            payload=initiate_payload,
            headers={"Content-Type": "application/json"},
        )
        if (
            not isinstance(plan, dict)
            or not plan.get("upload_id")
            or not plan.get("total_parts")
        ):
            raise RuntimeError(f"unexpected v2 initiate payload: {plan!r}")
        upload_id = str(plan["upload_id"])
        total_parts = int(plan["total_parts"])
        part_size = int(plan["part_size"])

        batch_entries = [{"part_number": number} for number in range(1, total_parts + 1)]
        batch_body = json.dumps({"parts": batch_entries}).encode()
        batch = self.request_json(
            "POST",
            f"/v2/uploads/{upload_id}/presign-batch",
            payload=batch_body,
            headers={"Content-Type": "application/json"},
        )
        if not isinstance(batch, dict) or not isinstance(batch.get("parts"), list):
            raise RuntimeError(f"unexpected v2 presign-batch payload: {batch!r}")

        completed_parts: list[dict[str, Any]] = []
        for part in batch["parts"]:
            number = int(part["number"])
            start = (number - 1) * part_size
            size = int(part["size"])
            chunk = payload[start : min(start + size, len(payload))]
            headers = {k: str(v) for k, v in (part.get("headers") or {}).items()}
            etag = self.put_s3_part(str(part["url"]), chunk, headers)
            completed_parts.append({"number": number, "etag": etag})

        complete_body = json.dumps({"parts": completed_parts}).encode()
        status, raw = self.request_status(
            "POST",
            f"/v2/uploads/{upload_id}/complete",
            payload=complete_body,
            headers={"Content-Type": "application/json"},
        )
        if status != 200:
            raise RuntimeError(
                f"v2 complete failed for {path}: status={status}, body={raw.decode(errors='replace')}"
            )
        result = self.wait_for_text_semantic_success(path)
        result.flow = "multipart_v2"
        return result


def assert_basic_semantic_text(flow_label: str, path: str, content_text: str) -> None:
    for want in (
        "semantic_text_format: drive9-file-semantic/v1",
        "purpose:",
        "key_topics:",
        "important_identifiers:",
        "structure:",
        "semantic_summary:",
    ):
        if want not in content_text:
            raise RuntimeError(f"{flow_label}: missing {want!r} in generated content_text")
    base_name = path.rsplit("/", 1)[-1]
    if base_name not in content_text:
        raise RuntimeError(
            f"{flow_label}: expected file basename {base_name!r} in content_text"
        )
    if "SemanticProbe" not in content_text:
        raise RuntimeError(
            f"{flow_label}: expected SemanticProbe identifier in content_text"
        )


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--mode",
        choices=("basic", "openai"),
        default="basic",
        help="basic: deterministic built-in generator assertions (default). "
        "openai: provider-backed runtime, only assert non-empty semantic text.",
    )
    parser.add_argument(
        "--base-url", default=DEFAULT_BASE_URL, help="drive9-server-local base URL"
    )
    parser.add_argument(
        "--timeout-seconds",
        type=float,
        default=90.0,
        help="wait timeout per flow / SQL poll",
    )
    parser.add_argument(
        "--poll-interval-seconds",
        type=float,
        default=1.0,
        help="poll interval while waiting for worker",
    )
    parser.add_argument(
        "--settle-seconds",
        type=float,
        default=12.0,
        help="seconds to wait before asserting no durable text semantic task",
    )
    parser.add_argument(
        "--text-file",
        help="only for --mode=openai: upload this local text file as the large-file payload; "
        "the file must be larger than the small-file threshold",
    )
    parser.add_argument(
        "--skip-direct",
        action="store_true",
        help="skip direct PUT flow",
    )
    parser.add_argument(
        "--skip-overwrite",
        action="store_true",
        help="skip overwrite flow",
    )
    parser.add_argument(
        "--skip-multipart-v1",
        action="store_true",
        help="skip v1 multipart flow",
    )
    parser.add_argument(
        "--skip-multipart-v2",
        action="store_true",
        help="skip v2 multipart flow",
    )
    parser.add_argument(
        "--skip-small-negative",
        action="store_true",
        help="skip the small direct-text negative check",
    )
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    if args.text_file and args.mode != "openai":
        raise RuntimeError("--text-file is only supported with --mode=openai")
    verifier = Verifier(args.base_url, args.timeout_seconds, args.poll_interval_seconds)
    custom_large_payload = read_large_text_file_payload(args.text_file) if args.text_file else None

    def assert_text(flow_label: str, path: str, got: str) -> None:
        if not got.strip():
            raise RuntimeError(f"{flow_label}: expected non-empty semantic text")
        if args.mode == "basic":
            assert_basic_semantic_text(flow_label, path, got)

    def large_payload(tag: str) -> bytes:
        if custom_large_payload is not None:
            return custom_large_payload
        return build_large_text_payload(tag)

    if not args.skip_direct:
        path = make_unique_path("direct")
        result = verifier.verify_direct_put_bytes(path, large_payload("direct"))
        assert_text("direct", path, result.content_text)
        print_result(result)

    if not args.skip_overwrite:
        path = make_unique_path("overwrite")
        first = verifier.verify_direct_put_bytes(path, large_payload("overwrite-a"))
        second = verifier.verify_direct_put_bytes(path, large_payload("overwrite-b"))
        if second.revision != first.revision + 1:
            raise RuntimeError(
                f"overwrite revision {first.revision} -> {second.revision}, want +1"
            )
        assert_text("overwrite", path, second.content_text)
        second.flow = "overwrite"
        print_result(second)

    if not args.skip_multipart_v1:
        path = make_unique_path("multipart-v1")
        result = verifier.verify_multipart_v1(path, large_payload("multipart-v1"))
        assert_text("multipart_v1", path, result.content_text)
        print_result(result)

    if not args.skip_multipart_v2:
        path = make_unique_path("multipart-v2")
        result = verifier.verify_multipart_v2(path, large_payload("multipart-v2"))
        assert_text("multipart_v2", path, result.content_text)
        print_result(result)

    if not args.skip_small_negative:
        path = make_unique_path("small-negative")
        payload = build_small_text_payload()
        verifier.put_file_best_effort(path, payload)
        verifier.assert_no_text_semantic_task(path, args.settle_seconds)
        content_text = verifier.wait_for_small_direct_text(path)
        if content_text != payload.decode("utf-8"):
            raise RuntimeError(
                f"small_negative content_text={content_text!r}, want {payload.decode('utf-8')!r}"
            )
        print(
            json.dumps(
                {
                    "ok": True,
                    "scenario": "small_direct_text_no_durable_task",
                    "path": path,
                    "content_text": content_text,
                },
                ensure_ascii=False,
            )
        )

    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as exc:  # pragma: no cover - CLI failure path
        print(json.dumps({"error": str(exc)}, ensure_ascii=False), file=sys.stderr)
        raise SystemExit(1)

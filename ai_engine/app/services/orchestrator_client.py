"""HTTP client for posting AIEngineResponse callbacks back to the orchestrator.

Go's json.Marshal serialises []byte as base64 and expects the same on unmarshal,
so Tests must be sent as a base64-encoded string.  All top-level keys must be
capitalised (no json struct tags on AIEngineResponse), while nested PullRequest
fields use lowercase (they do have json tags).
"""

from __future__ import annotations

import base64
import hashlib
import hmac
import json
import logging

import httpx

from app.core.config import settings

logger = logging.getLogger(__name__)

_AI_ENGINE_CALLBACK_PATH = "/aiengine"


def _sign(body: bytes) -> str:
    return hmac.new(settings.ai_engine_secret.encode(), body, hashlib.sha256).hexdigest()


async def send_response(
    wfid: int,
    pull_request: dict,
    done: bool,
    *,
    test_name: str = "",
    tests: bytes = b"",
    test_cmd: list[str] | None = None,
    summary: str = "",
) -> None:
    """POST an AIEngineResponse to the orchestrator /aiengine endpoint."""
    payload = {
        "Wfid": wfid,
        "PullRequest": pull_request,
        "Done": done,
        "TestName": test_name,
        "Tests": base64.b64encode(tests).decode() if tests else "",
        "TestCmd": test_cmd or [],
        "Summary": summary,
    }
    body = json.dumps(payload).encode()

    url = f"{settings.orchestrator_url}{_AI_ENGINE_CALLBACK_PATH}"
    async with httpx.AsyncClient(timeout=10) as client:
        resp = await client.post(
            url,
            content=body,
            headers={
                "Content-Type": "application/json",
                "HMAC-Signature-256": _sign(body),
            },
        )
    if resp.status_code != 200:
        logger.error(
            "Orchestrator callback failed: status=%s body=%s",
            resp.status_code,
            resp.text,
        )

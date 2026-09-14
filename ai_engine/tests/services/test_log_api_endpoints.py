import hashlib
import hmac
import json
from types import SimpleNamespace

import pytest
from fastapi.testclient import TestClient

from app.main import app
from app.core.config import settings
from app.services.log_response import create_log_response


client = TestClient(app)


@pytest.fixture(autouse=True)
def celery_task(monkeypatch):
    job_id = "550e8400-e29b-41d4-a716-446655440000"
    tasks_by_job_id = {}

    def fake_delay(raw_log):
        task = SimpleNamespace(
            id=job_id,
            state="SUCCESS",
            ready=lambda: True,
            result=create_log_response(raw_log),
        )
        tasks_by_job_id[job_id] = task
        return task

    monkeypatch.setattr("app.main.analyze_log.delay", fake_delay)
    monkeypatch.setattr("app.main.celery_app.AsyncResult", lambda requested_job_id: tasks_by_job_id[requested_job_id])
    return tasks_by_job_id


def test_analyze_accepts_log_and_returns_job_id():
    payload = {
        "raw_log": "AssertionError: boom",
        "source": "user",
    }

    response = client.post("/analyze", json=payload)

    assert response.status_code == 202
    body = response.json()
    assert body["status"] == "accepted"
    assert isinstance(body["job_id"], str)
    assert len(body["job_id"]) > 10


def test_result_endpoint_returns_user_facing_schema():
    payload = {
        "raw_log": "Traceback (most recent call last):\nAssertionError: divide by zero\n",
        "source": "ci",
    }

    accepted = client.post("/analyze", json=payload)
    job_id = accepted.json()["job_id"]

    result_response = client.get(f"/result/{job_id}")
    assert result_response.status_code == 200

    body = result_response.json()
    assert body["status"] == "completed"
    assert body["job_id"] == job_id

    result = body["result"]
    required_keys = {
        "error_type",
        "failing_test",
        "stack_trace_lines",
        "error_signature",
        "language",
        "framework",
        "confidence",
        "root_cause_message",
        "parser_version",
        "fallback_reason",
    }
    assert required_keys.issubset(set(result.keys()))


def test_result_endpoint_redacts_sensitive_tokens():
    payload = {
        "raw_log": (
            "TypeError: secret=abc123 at /Users/demo/project/file.py "
            "request_id=550e8400-e29b-41d4-a716-446655440000"
        ),
        "source": "user",
    }

    accepted = client.post("/analyze", json=payload)
    job_id = accepted.json()["job_id"]

    result = client.get(f"/result/{job_id}").json()["result"]
    signature = result["error_signature"]

    assert "<secret>" in signature
    assert "<path>" in signature
    assert "<uuid>" in signature


@pytest.mark.parametrize("job_type", ["open", "edit", "sync"])
def test_lifecycle_jobs_are_accepted_and_queued(job_type, monkeypatch):
    body = json.dumps({"Wfid": 1}).encode()
    monkeypatch.setattr(settings, "ai_engine_secret", "test-secret")
    sent_responses = []

    async def fake_send_response(*args, **kwargs):
        sent_responses.append((args, kwargs))

    monkeypatch.setattr(
        "app.services.orchestrator_client.send_response",
        fake_send_response,
    )
    monkeypatch.setattr(
        "app.services.repo_test_gen.generate_initial_test",
        lambda repo_context, pull_request, failure_log="": {
            "test_name": "tests/generated/test_pr.py",
            "test_cmd": ["pytest", "tests/generated/test_pr.py"],
            "test_code": "def test_pr():\n    assert True\n",
        },
    )
    signature = hmac.new(settings.ai_engine_secret.encode(), body, hashlib.sha256).hexdigest()

    response = client.post(
        "/",
        content=body,
        headers={"Job-Type": job_type, "HMAC-Signature-256": signature},
    )

    assert response.status_code == 200
    assert response.json() == {"status": "accepted"}
    if job_type == "open":
        assert sent_responses[0][1]["done"] is False
        assert sent_responses[0][1]["test_name"] == "tests/generated/test_pr.py"
        assert sent_responses[0][1]["test_cmd"] == ["pytest", "tests/generated/test_pr.py"]
        assert sent_responses[0][1]["tests"]

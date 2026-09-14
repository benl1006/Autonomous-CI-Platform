from __future__ import annotations

import hashlib
import hmac
from typing import Literal

from fastapi import BackgroundTasks, FastAPI, HTTPException, Request
from pydantic import BaseModel, Field, ValidationError

from app.core.config import settings
from app.tasks.workers import analyze_log, celery_app


class AnalyzeRequest(BaseModel):
    """Incoming payload for log analysis requests."""

    raw_log: str = Field(..., min_length=1)
    source: Literal["ci", "user"] = "ci"


class ParsedLogResponse(BaseModel):
    """User-facing structured log analysis response."""

    error_type: str
    failing_test: str
    stack_trace_lines: list[str]
    error_signature: str
    language: Literal["python", "node", "go", "java", "rust", "unknown"]
    framework: str
    confidence: float
    root_cause_message: str
    parser_version: str
    fallback_reason: str


class GenerateTestsRequest(BaseModel):
    """Incoming payload for test-generation requests."""

    raw_log: str = Field(..., min_length=1)
    source: Literal["ci", "user"] = "ci"
    fixture_name: str | None = Field(None, min_length=1, max_length=80)


class GeneratedTestsResponse(BaseModel):
    """Response returned by POST /generate-tests."""

    fixture_slug: str
    expected_json: dict
    metadata_json: dict
    test_stub: str
    documentation: str
    saved_to_disk: bool
    fixture_path: str


class AnalyzeAcceptedResponse(BaseModel):
    """Acknowledgement returned when analysis is accepted."""

    job_id: str
    status: Literal["accepted"]


class AnalyzeResultResponse(BaseModel):
    """Result payload returned for completed analysis jobs."""

    job_id: str
    status: Literal["completed"]
    result: ParsedLogResponse


app = FastAPI(
    title="AI Engine",
    description="AI/Data Processing service for the Self-Healing CI/CD Platform",
    version="0.1.0",
)


# ---------------------------------------------------------------------------
# Job endpoint — called by the Go orchestrator with a Job-Type header
# ---------------------------------------------------------------------------

VALID_JOB_TYPES = frozenset({"open", "close", "edit", "sync", "logs"})


def _verify_hmac(body: bytes, signature: str) -> bool:
    """Check body against the HMAC-Signature-256 header the orchestrator sends."""
    if not signature:
        return False
    expected = hmac.new(settings.ai_engine_secret.encode(), body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(expected, signature)


class _PullRequestPayload(BaseModel):
    number: int = 0
    action: str = ""
    branch: str = ""
    title: str = ""
    body: str = ""
    headsha: str = ""
    basesha: str = ""
    merged: bool = False
    url: str = ""


class JobRequest(BaseModel):
    """Mirrors the Go AIEngineRequest struct (no json tags → capitalised keys)."""

    Wfid: int
    RepoUrl: str = ""
    PullRequest: _PullRequestPayload = Field(default_factory=_PullRequestPayload)
    Stdout: str = ""
    Stderr: str = ""
    StartTime: str = ""
    EndTime: str = ""
    Errors: str = ""
    Status: str = ""
    OOMKilled: bool = False
    ExitCode: int = 0


async def _process_job(job_type: str, payload: JobRequest) -> None:
    """Background task: analyse logs and POST the result back to the orchestrator."""
    from app.services import orchestrator_client
    from app.services.repo_test_gen import generate_initial_test

    pr = payload.PullRequest
    pr_dict = {
        "number": pr.number,
        "action": pr.action,
        "branch": pr.branch,
        "title": pr.title,
        "body": pr.body,
        "headsha": pr.headsha,
        "basesha": pr.basesha,
        "merged": pr.merged,
        "url": pr.url,
    }

    if job_type == "open":
        package = generate_initial_test(payload.RepoUrl, pr.headsha, pr_dict)
        await orchestrator_client.send_response(
            payload.Wfid,
            pr_dict,
            done=False,
            test_name=package["test_name"],
            tests=package["test_code"].encode(),
            test_cmd=package["test_cmd"],
        )
        return

    if job_type in {"edit", "sync"}:
        return

    if job_type == "close":
        await orchestrator_client.send_response(
            payload.Wfid,
            pr_dict,
            done=True,
            summary=f"Workflow {payload.Wfid} closed for PR #{pr.number}.",
        )
        return

    # job_type == "logs"
    if payload.ExitCode == 0:
        await orchestrator_client.send_response(
            payload.Wfid,
            pr_dict,
            done=True,
            summary=f"All tests passed on PR #{pr.number} (exit 0).",
        )
        return

    combined_log = "\n".join(filter(None, [payload.Stdout, payload.Stderr, payload.Errors]))
    package = generate_initial_test(payload.RepoUrl, pr.headsha, pr_dict, failure_log=combined_log)
    await orchestrator_client.send_response(
        payload.Wfid,
        pr_dict,
        done=False,
        test_name=package["test_name"],
        tests=package["test_code"].encode(),
        test_cmd=package["test_cmd"],
    )


@app.post("/")
async def job_handler(request: Request, background_tasks: BackgroundTasks) -> dict:
    """Receive a job from the orchestrator, acknowledge immediately, process in background."""
    job_type = request.headers.get("Job-Type", "")
    if job_type not in VALID_JOB_TYPES:
        raise HTTPException(status_code=400, detail=f"Unknown Job-Type: {job_type!r}")

    body = await request.body()
    if not _verify_hmac(body, request.headers.get("HMAC-Signature-256", "")):
        raise HTTPException(status_code=401, detail="Invalid or missing HMAC-Signature-256")

    try:
        payload = JobRequest.model_validate_json(body)
    except ValidationError as exc:
        raise HTTPException(status_code=422, detail=exc.errors()) from exc

    background_tasks.add_task(_process_job, job_type, payload)
    return {"status": "accepted"}


@app.get("/health")
async def health_check():
    """Liveness probe — returns 200 when the service is running."""
    return {"status": "ok"}


@app.post("/analyze", status_code=202, response_model=AnalyzeAcceptedResponse)
async def analyze(payload: AnalyzeRequest) -> AnalyzeAcceptedResponse:
    """Queue log analysis for a Celery worker and return its task identifier."""
    task = analyze_log.delay(payload.raw_log)
    return AnalyzeAcceptedResponse(job_id=task.id, status="accepted")


@app.get("/result/{job_id}", response_model=AnalyzeResultResponse)
async def get_result(job_id: str) -> AnalyzeResultResponse:
    """Return a completed analysis result from the Celery Redis backend."""
    task = celery_app.AsyncResult(job_id)
    if task.state == "PENDING":
        raise HTTPException(status_code=202, detail="analysis task is still pending")
    if task.state == "FAILURE":
        raise HTTPException(status_code=500, detail="analysis task failed")
    if not task.ready():
        raise HTTPException(status_code=202, detail="analysis task is still running")
    return AnalyzeResultResponse(job_id=job_id, status="completed", result=task.result)


@app.post("/generate-tests", response_model=GeneratedTestsResponse)
async def generate_tests_endpoint(payload: GenerateTestsRequest) -> GeneratedTestsResponse:
    """Analyze a failure log and return a fixture package + pytest stub the user can run immediately."""
    from app.services.test_gen import generate_tests

    package = generate_tests(
        payload.raw_log,
        source=payload.source,
        fixture_name=payload.fixture_name,
    )
    return GeneratedTestsResponse(
        fixture_slug=package["fixture_slug"],
        expected_json=package["expected_json"],
        metadata_json=package["metadata_json"],
        test_stub=package["test_stub"],
        documentation=package["documentation"],
        saved_to_disk=package["saved_to_disk"],
        fixture_path=package["fixture_path"],
    )

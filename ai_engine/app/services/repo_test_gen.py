"""Generate an initial regression test from a checked-out PR context."""

from __future__ import annotations

import json
import os
import re
import shutil
import subprocess
import tempfile
from contextlib import contextmanager
from pathlib import Path
from typing import Iterator
import base64

from google import genai

from app.core.config import settings


@contextmanager
def _checkout_repository(repo_url: str, head_sha: str) -> Iterator[Path]:
    """Fetch one immutable PR commit into a temporary directory."""
    if not repo_url:
        raise RuntimeError("RepoUrl is required for initial PR test generation")
    if not re.fullmatch(r"[0-9a-fA-F]{7,64}", head_sha):
        raise RuntimeError("PullRequest.headsha must be a valid commit SHA")

    path = Path(tempfile.mkdtemp(prefix="ai-pr-"))
    env = os.environ.copy()
    if settings.github_pat:
        auth = base64.b64encode(f":{settings.github_pat}".encode()).decode()
        env.update(
            {
                "GIT_CONFIG_COUNT": "1",
                "GIT_CONFIG_KEY_0": "http.extraHeader",
                "GIT_CONFIG_VALUE_0": f"Authorization: Basic {auth}",
            }
        )
    try:
        commands = [
            ["git", "init", str(path)],
            ["git", "-C", str(path), "remote", "add", "origin", repo_url],
            ["git", "-C", str(path), "fetch", "--depth", "1", "origin", head_sha],
            ["git", "-C", str(path), "checkout", "--detach", "FETCH_HEAD"],
        ]
        for command in commands:
            subprocess.run(command, check=True, capture_output=True, text=True, env=env, timeout=120)
        yield path
    except (OSError, subprocess.SubprocessError) as exc:
        raise RuntimeError("Failed to clone the PR commit for test generation") from exc
    finally:
        shutil.rmtree(path, ignore_errors=True)


def _repository_context(root: Path, max_bytes: int = 1_000_000) -> str:
    """Read tracked source/config files from the local PR checkout for Gemini."""
    chunks: list[str] = []
    total = 0
    result = subprocess.run(
        ["git", "-C", str(root), "ls-files"],
        check=True,
        capture_output=True,
        text=True,
        timeout=30,
    )
    for relative_name in result.stdout.splitlines():
        path = root / relative_name
        if path.suffix.lower() not in {
            ".py", ".go", ".js", ".jsx", ".ts", ".tsx", ".java", ".rs", ".rb", ".php", ".cs",
            ".json", ".toml", ".yaml", ".yml", ".md",
        } and path.name not in {"Dockerfile", "Makefile"}:
            continue
        try:
            content = path.read_bytes()
        except OSError:
            continue
        if b"\x00" in content:
            continue
        remaining = max_bytes - total
        if remaining <= 0:
            break
        content = content[:remaining]
        chunks.append(f"\n--- {relative_name} ---\n{content.decode(errors='replace')}\n")
        total += len(content)
    return "".join(chunks)


def generate_initial_test(
    repo_url: str,
    head_sha: str,
    pull_request: dict,
    failure_log: str = "",
) -> dict[str, str | list[str]]:
    """Ask Gemini for one executable test file and its command."""
    if not settings.gemini_api_key:
        raise RuntimeError("GEMINI_API_KEY is required for initial PR test generation")

    with _checkout_repository(repo_url, head_sha) as repository:
        repo_context = _repository_context(repository)
        prompt = f"""
You are generating the first regression test for a pull request.
Return JSON only with exactly these keys:
  test_name: relative path under tests/generated, ending in .py
  test_cmd: array of command strings that runs only this generated test
  test_code: complete test file contents

Rules:
- Use the repository's existing language, test framework, and import paths.
- Test behavior changed or introduced by the pull request, not the AI platform.
- Do not modify application files or require network access.
- Keep the test deterministic and self-contained.
- Use pytest when the repository is Python; otherwise use the repository's existing test runner.
- The test_name must be a safe relative path below tests/generated.

Pull request:
{json.dumps(pull_request, indent=2)}

Repository context:
{repo_context}

Previous test failure (empty for the first test):
{failure_log}
"""
        client = genai.Client(api_key=settings.gemini_api_key)
        response = client.models.generate_content(model=settings.gemini_model, contents=prompt)
    raw = (response.text or "").strip()
    raw = re.sub(r"^```(?:json)?\s*|\s*```$", "", raw, flags=re.IGNORECASE)
    try:
        result = json.loads(raw)
    except json.JSONDecodeError as exc:
        raise RuntimeError("Gemini returned invalid initial test JSON") from exc

    test_name = result.get("test_name")
    test_cmd = result.get("test_cmd")
    test_code = result.get("test_code")
    if (
        not isinstance(test_name, str)
        or not test_name.startswith("tests/generated/")
        or ".." in test_name.split("/")
        or not test_name.endswith(".py")
        or not isinstance(test_cmd, list)
        or not all(isinstance(item, str) and item for item in test_cmd)
        or not isinstance(test_code, str)
        or not test_code.strip()
    ):
        raise RuntimeError("Gemini returned an invalid initial test package")

    return {"test_name": test_name, "test_cmd": test_cmd, "test_code": test_code}
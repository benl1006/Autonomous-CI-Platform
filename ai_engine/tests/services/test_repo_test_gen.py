import subprocess

import pytest

from app.services.repo_test_gen import _checkout_repository


def test_checkout_repository_uses_requested_commit(tmp_path):
    source = tmp_path / "source"
    source.mkdir()
    subprocess.run(["git", "init", str(source)], check=True, capture_output=True)
    subprocess.run(["git", "-C", str(source), "config", "user.email", "test@example.com"], check=True)
    subprocess.run(["git", "-C", str(source), "config", "user.name", "Test"], check=True)

    first_file = source / "value.txt"
    first_file.write_text("first\n")
    subprocess.run(["git", "-C", str(source), "add", "."], check=True)
    subprocess.run(["git", "-C", str(source), "commit", "-m", "first"], check=True, capture_output=True)
    first_sha = subprocess.check_output(["git", "-C", str(source), "rev-parse", "HEAD"], text=True).strip()

    first_file.write_text("second\n")
    subprocess.run(["git", "-C", str(source), "commit", "-am", "second"], check=True, capture_output=True)

    with _checkout_repository(str(source), first_sha) as checkout:
        checked_out_sha = subprocess.check_output(
            ["git", "-C", str(checkout), "rev-parse", "HEAD"],
            text=True,
        ).strip()
        assert checked_out_sha == first_sha
        assert (checkout / "value.txt").read_text() == "first\n"


def test_checkout_repository_rejects_invalid_sha():
    with pytest.raises(RuntimeError, match="valid commit SHA"):
        with _checkout_repository("https://example.com/repo.git", "not-a-sha"):
            pass
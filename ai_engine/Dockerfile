# syntax=docker/dockerfile:1
FROM python:3.11-slim

WORKDIR /app

# Install dependencies first — Docker caches this layer separately from source
# code, so a code-only change won't trigger a full pip install. The cache
# mount persists downloaded wheels across builds, so a requirements.txt change
# (e.g. adding one package) doesn't force re-downloading everything else.
COPY requirements.txt .
RUN --mount=type=cache,target=/root/.cache/pip pip install -r requirements.txt

# Copy application source
COPY . .

EXPOSE 8000

# Default command runs the FastAPI service, honoring AI_ENGINE_PORT so the
# container's listen port always matches docker-compose's port mapping.
# The celery-worker service in docker-compose overrides this CMD.
CMD ["sh", "-c", "uvicorn app.main:app --host 0.0.0.0 --port ${AI_ENGINE_PORT:-8000} --reload"]

from pydantic import Field
from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    """
    Reads configuration from environment variables or an optional .env file.
    All values can be overridden via docker-compose environment: blocks.
    """

    model_config = SettingsConfigDict(
        env_file=".env",
        env_file_encoding="utf-8",
        extra="ignore",
    )

    # PostgreSQL — asyncpg driver is required for SQLAlchemy async sessions.
    # Format: postgresql+asyncpg://user:password@host:port/dbname
    database_url: str = "postgresql+asyncpg://postgres:postgres@localhost:5432/aiplatform"

    # Redis — used as Celery broker and result backend.
    # Format: redis://host:port/db_index
    redis_url: str = "redis://localhost:6379/0"

    # Gemini API key for Google Gemini — used by the patch generator (Phase 4).
    gemini_api_key: str = ""

    # sentence-transformers model used to create vector embeddings.
    # all-MiniLM-L6-v2 produces 384-dimensional vectors.
    embedding_model: str = "all-MiniLM-L6-v2"
    embedding_dimensions: int = 384

    # Orchestrator callback — where to POST AIEngineResponse after processing a job.
    orchestrator_url: str = "http://localhost:8080"

    # Shared secret for HMAC-signing/verifying messages exchanged with the
    # orchestrator. Env var is INTERNAL_SECRET to match orchestrator/.env's
    # INTERNAL_SECRET exactly (both sides must use the same value).
    ai_engine_secret: str = Field(default="", validation_alias="INTERNAL_SECRET")


settings = Settings()

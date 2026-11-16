"""Typed errors for the OpenRouter connector.

The caller (core-api via ai-engine) treats every upstream response as
untrusted; each failure mode maps to a distinct exception so it can
decide whether to retry, degrade or abort the pipeline stage.
"""

from __future__ import annotations

from connectors.openrouter.models import ErrorBody


class OpenRouterError(Exception):
    """Base class for every error raised by the openrouter connector."""


class OpenRouterConfigError(OpenRouterError):
    """Missing or invalid connector configuration (env vars, URLs)."""


class OpenRouterTransportError(OpenRouterError):
    """Network-level failure: DNS, TLS, connect/read timeout, protocol errors."""


class OpenRouterSchemaError(OpenRouterError):
    """A success body violating the expected schema (incl. wrong embed dims)."""


class OpenRouterApiError(OpenRouterError):
    """Non-2xx response; carries the HTTP status and parsed ``error`` body."""

    def __init__(
        self,
        message: str,
        *,
        status: int,
        error: ErrorBody | None = None,
    ) -> None:
        super().__init__(message)
        self.status = status
        self.error = error


class OpenRouterAuthError(OpenRouterApiError):
    """401/403: the API key was rejected by OpenRouter."""

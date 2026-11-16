"""Typed errors for the Hapag-Lloyd connector.

The caller (core-api via ai-engine) treats every upstream response as untrusted;
each failure mode maps to a distinct exception so it can decide whether to
retry, degrade or abort the pricing stage.
"""

from __future__ import annotations

from connectors.hapag_lloyd.models import Problem


class HapagError(Exception):
    """Base class for every error raised by the hapag_lloyd connector."""


class HapagConfigError(HapagError):
    """Missing or invalid connector configuration (env vars, base URL)."""


class HapagTransportError(HapagError):
    """Network-level failure: DNS, TLS, connect/read timeout, protocol errors."""


class HapagSchemaError(HapagError):
    """A success response body that violates the published OpenAPI schema."""


class HapagApiError(HapagError):
    """Non-2xx response; carries the HTTP status and parsed RFC 7807 Problem."""

    def __init__(
        self,
        message: str,
        *,
        status: int,
        problem: Problem | None = None,
    ) -> None:
        super().__init__(message)
        self.status = status
        self.problem = problem


class HapagAuthError(HapagApiError):
    """401/403: the client credentials were rejected by the API portal."""

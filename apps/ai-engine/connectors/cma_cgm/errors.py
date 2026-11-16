"""Typed errors for the CMA CGM connector.

The caller (core-api via ai-engine) treats every upstream response as untrusted;
each failure mode maps to a distinct exception so it can decide whether to
retry, degrade or abort the pricing stage.
"""

from __future__ import annotations

from connectors.cma_cgm.models import Fault


class CmaCgmError(Exception):
    """Base class for every error raised by the cma_cgm connector."""


class CmaCgmConfigError(CmaCgmError):
    """Missing or invalid connector configuration (env vars, base URL)."""


class CmaCgmTransportError(CmaCgmError):
    """Network-level failure: DNS, TLS, connect/read timeout, protocol errors."""


class CmaCgmSchemaError(CmaCgmError):
    """A success response body violating the published OpenAPI schema."""


class CmaCgmApiError(CmaCgmError):
    """Non-2xx response; carries the HTTP status and parsed ``Fault`` body."""

    def __init__(
        self,
        message: str,
        *,
        status: int,
        fault: Fault | None = None,
    ) -> None:
        super().__init__(message)
        self.status = status
        self.fault = fault


class CmaCgmAuthError(CmaCgmApiError):
    """Rejected credentials: token endpoint non-200 or search 401/403."""

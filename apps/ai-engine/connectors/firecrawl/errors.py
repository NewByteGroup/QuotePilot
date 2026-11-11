"""Typed errors for the Firecrawl connector.

The caller (core-api via ai-engine) treats every upstream response as untrusted;
each failure mode maps to a distinct exception so it can decide whether to
retry, degrade or abort the pricing stage.
"""

from __future__ import annotations

from connectors.firecrawl.models import ScrapeErrorBody


class FirecrawlError(Exception):
    """Base class for every error raised by the firecrawl connector."""


class FirecrawlConfigError(FirecrawlError):
    """Missing or invalid connector configuration (env vars, base URL)."""


class FirecrawlTransportError(FirecrawlError):
    """Network-level failure: DNS, TLS, connect/read timeout, protocol errors."""


class FirecrawlSchemaError(FirecrawlError):
    """A success response body violating the expected scrape envelope."""


class FirecrawlApiError(FirecrawlError):
    """Non-2xx response; carries the HTTP status and parsed error body."""

    def __init__(
        self,
        message: str,
        *,
        status: int,
        body: ScrapeErrorBody | None = None,
    ) -> None:
        super().__init__(message)
        self.status = status
        self.body = body


class FirecrawlAuthError(FirecrawlApiError):
    """401/403: the API key was rejected by Firecrawl."""

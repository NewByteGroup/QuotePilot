"""Firecrawl v2 connector (scrape → markdown).

Public surface: ``FirecrawlClient`` plus the result/metadata models it
exchanges. Everything here is untrusted-input aware: responses are validated
against the expected scrape envelope before reaching the caller.
"""

from connectors.firecrawl.client import (
    DEFAULT_TIMEOUT_SECONDS,
    PROD_BASE_URL,
    FirecrawlClient,
    ScrapeResult,
)
from connectors.firecrawl.errors import (
    FirecrawlApiError,
    FirecrawlAuthError,
    FirecrawlConfigError,
    FirecrawlError,
    FirecrawlSchemaError,
    FirecrawlTransportError,
)
from connectors.firecrawl.models import (
    CamelModel,
    ScrapeData,
    ScrapeErrorBody,
    ScrapeMetadata,
    ScrapeResponse,
)

__all__ = [
    "DEFAULT_TIMEOUT_SECONDS",
    "PROD_BASE_URL",
    "CamelModel",
    "FirecrawlApiError",
    "FirecrawlAuthError",
    "FirecrawlClient",
    "FirecrawlConfigError",
    "FirecrawlError",
    "FirecrawlSchemaError",
    "FirecrawlTransportError",
    "ScrapeData",
    "ScrapeErrorBody",
    "ScrapeMetadata",
    "ScrapeResponse",
    "ScrapeResult",
]

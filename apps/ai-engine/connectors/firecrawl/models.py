"""Pydantic models for the Firecrawl v2 ``POST /scrape`` API.

The response is untrusted input: it is schema-validated into pydantic models
before reaching the caller, per the untrusted-response rule in AGENTS.md.

A note on semantics: Firecrawl answers HTTP 200 with ``success: true`` even
when the *target* page failed — the scraped page's own status lives in
``data.metadata.statusCode``. Callers must check that field (exposed as
``ScrapeResult.page_failed`` on the client) before trusting the markdown.
"""

from __future__ import annotations

from pydantic import BaseModel, ConfigDict, Field
from pydantic.alias_generators import to_camel


class CamelModel(BaseModel):
    """Base model: camelCase JSON aliases, immutable, unknown keys ignored."""

    model_config = ConfigDict(
        alias_generator=to_camel,
        populate_by_name=True,
        extra="ignore",
        frozen=True,
    )


class ScrapeMetadata(CamelModel):
    """Subset of ``data.metadata`` on a ``POST /scrape`` reply.

    ``status_code`` is the *target page's* HTTP status, not Firecrawl's own;
    it is ``None`` when upstream omits it. Remaining metadata fields
    (language, og:* tags, scrapeId, creditsUsed, ...) are ignored.
    """

    status_code: int | None = None
    title: str | None = None
    url: str | None = None
    description: str | None = None


class ScrapeData(CamelModel):
    """``data`` payload of a successful ``POST /scrape`` reply."""

    markdown: str
    metadata: ScrapeMetadata = Field(default_factory=ScrapeMetadata)


class ScrapeResponse(CamelModel):
    """Envelope of a ``200`` reply to ``POST /scrape``.

    ``success`` and ``data`` are required: a 200 without them is malformed
    and surfaces as ``FirecrawlSchemaError`` at the client boundary.
    """

    success: bool
    data: ScrapeData


class ScrapeErrorBody(CamelModel):
    """Leniently parsed error body on non-2xx replies.

    v2 uses ``{"success": false, "error": "..."}``; some variants carry
    ``{"code": "...", "error": "..."}`` or a ``message`` key instead.
    All fields are optional so any shape parses.
    """

    success: bool | None = None
    error: str | None = None
    code: str | None = None
    message: str | None = None

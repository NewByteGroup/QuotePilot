"""Shared test helpers: fixture loading and named fakes (no live network)."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import httpx

FIXTURES_DIR = Path(__file__).resolve().parent / "fixtures"


def fixture_text(*parts: str) -> str:
    """Return the raw text of a fixture under ``tests/fixtures/``."""
    return FIXTURES_DIR.joinpath(*parts).read_text(encoding="utf-8")


def fixture_json(*parts: str) -> Any:
    """Return a fixture under ``tests/fixtures/`` parsed as JSON."""
    return json.loads(fixture_text(*parts))


class FakeHapagTransport(httpx.AsyncBaseTransport):
    """Named fake HTTP transport for connector tests.

    Records every request and replies with a canned status/body, or raises the
    configured error. Keeps tests free of live network calls (ADR-008).
    """

    def __init__(
        self,
        *,
        status: int = 201,
        body: str = "{}",
        headers: dict[str, str] | None = None,
        error: Exception | None = None,
    ) -> None:
        self.status = status
        self.body = body
        self.headers = headers or {"Content-Type": "application/json"}
        self.error = error
        self.requests: list[httpx.Request] = []

    async def handle_async_request(self, request: httpx.Request) -> httpx.Response:
        self.requests.append(request)
        if self.error is not None:
            raise self.error
        return httpx.Response(
            self.status,
            content=self.body.encode("utf-8"),
            headers=self.headers,
            request=request,
        )


class FakeFirecrawlTransport(httpx.AsyncBaseTransport):
    """Named fake HTTP transport for the firecrawl connector tests.

    Records every request and replies with a canned status/body, or raises the
    configured error. Keeps tests free of live network calls (ADR-008).
    """

    def __init__(
        self,
        *,
        status: int = 200,
        body: str = "{}",
        headers: dict[str, str] | None = None,
        error: Exception | None = None,
    ) -> None:
        self.status = status
        self.body = body
        self.headers = headers or {"Content-Type": "application/json"}
        self.error = error
        self.requests: list[httpx.Request] = []

    async def handle_async_request(self, request: httpx.Request) -> httpx.Response:
        self.requests.append(request)
        if self.error is not None:
            raise self.error
        return httpx.Response(
            self.status,
            content=self.body.encode("utf-8"),
            headers=self.headers,
            request=request,
        )


class FakeCmaCgmTransport(httpx.AsyncBaseTransport):
    """Named fake HTTP transport for the CMA CGM connector tests.

    Dispatches canned responses per endpoint: the OAuth token URL gets the
    ``token_*`` reply, everything else gets the ``search_*`` reply (or raises
    the matching configured error). Records every request; tests inspect
    ``token_requests``/``search_requests`` separately.
    """

    def __init__(
        self,
        *,
        token_status: int = 200,
        token_body: str = "{}",
        token_headers: dict[str, str] | None = None,
        token_error: Exception | None = None,
        search_status: int = 200,
        search_body: str = "[]",
        search_headers: dict[str, str] | None = None,
        search_error: Exception | None = None,
    ) -> None:
        self.token_status = token_status
        self.token_body = token_body
        self.token_headers = token_headers or {"Content-Type": "application/json"}
        self.token_error = token_error
        self.search_status = search_status
        self.search_body = search_body
        self.search_headers = search_headers or {"Content-Type": "application/json"}
        self.search_error = search_error
        self.requests: list[httpx.Request] = []

    @property
    def token_requests(self) -> list[httpx.Request]:
        return [r for r in self.requests if self._is_token_request(r)]

    @property
    def search_requests(self) -> list[httpx.Request]:
        return [r for r in self.requests if not self._is_token_request(r)]

    @staticmethod
    def _is_token_request(request: httpx.Request) -> bool:
        return request.url.path.endswith("/token.oauth2")

    async def handle_async_request(self, request: httpx.Request) -> httpx.Response:
        self.requests.append(request)
        if self._is_token_request(request):
            return self._reply(
                self.token_status,
                self.token_body,
                self.token_headers,
                self.token_error,
                request,
            )
        return self._reply(
            self.search_status,
            self.search_body,
            self.search_headers,
            self.search_error,
            request,
        )

    @staticmethod
    def _reply(
        status: int,
        body: str,
        headers: dict[str, str],
        error: Exception | None,
        request: httpx.Request,
    ) -> httpx.Response:
        if error is not None:
            raise error
        return httpx.Response(
            status,
            content=body.encode("utf-8"),
            headers=headers,
            request=request,
        )


class FakeOpenRouterTransport(httpx.AsyncBaseTransport):
    """Named fake HTTP transport for the OpenRouter connector tests.

    Dispatches canned responses per endpoint: ``*/decisions`` gets the
    ``decisions_*`` reply, ``*/chat/completions`` the ``chat_*`` reply and
    ``*/embeddings`` the ``embeddings_*`` reply (each or raise its
    configured error). Records every request; tests inspect the
    per-endpoint ``*_requests`` lists.
    """

    def __init__(
        self,
        *,
        decisions_status: int = 200,
        decisions_body: str = "{}",
        decisions_headers: dict[str, str] | None = None,
        decisions_error: Exception | None = None,
        chat_status: int = 200,
        chat_body: str = "{}",
        chat_headers: dict[str, str] | None = None,
        chat_error: Exception | None = None,
        embeddings_status: int = 200,
        embeddings_body: str = "{}",
        embeddings_headers: dict[str, str] | None = None,
        embeddings_error: Exception | None = None,
    ) -> None:
        self.decisions_status = decisions_status
        self.decisions_body = decisions_body
        self.decisions_headers = decisions_headers or {
            "Content-Type": "application/json"
        }
        self.decisions_error = decisions_error
        self.chat_status = chat_status
        self.chat_body = chat_body
        self.chat_headers = chat_headers or {"Content-Type": "application/json"}
        self.chat_error = chat_error
        self.embeddings_status = embeddings_status
        self.embeddings_body = embeddings_body
        self.embeddings_headers = embeddings_headers or {
            "Content-Type": "application/json"
        }
        self.embeddings_error = embeddings_error
        self.requests: list[httpx.Request] = []

    @staticmethod
    def _endpoint(request: httpx.Request) -> str:
        path = request.url.path
        if path.endswith("/decisions"):
            return "decisions"
        if path.endswith("/chat/completions"):
            return "chat"
        return "embeddings"

    @property
    def decisions_requests(self) -> list[httpx.Request]:
        return [r for r in self.requests if self._endpoint(r) == "decisions"]

    @property
    def chat_requests(self) -> list[httpx.Request]:
        return [r for r in self.requests if self._endpoint(r) == "chat"]

    @property
    def embeddings_requests(self) -> list[httpx.Request]:
        return [r for r in self.requests if self._endpoint(r) == "embeddings"]

    async def handle_async_request(self, request: httpx.Request) -> httpx.Response:
        self.requests.append(request)
        endpoint = self._endpoint(request)
        if endpoint == "decisions":
            return self._reply(
                self.decisions_status,
                self.decisions_body,
                self.decisions_headers,
                self.decisions_error,
                request,
            )
        if endpoint == "chat":
            return self._reply(
                self.chat_status,
                self.chat_body,
                self.chat_headers,
                self.chat_error,
                request,
            )
        return self._reply(
            self.embeddings_status,
            self.embeddings_body,
            self.embeddings_headers,
            self.embeddings_error,
            request,
        )

    @staticmethod
    def _reply(
        status: int,
        body: str,
        headers: dict[str, str],
        error: Exception | None,
        request: httpx.Request,
    ) -> httpx.Response:
        if error is not None:
            raise error
        return httpx.Response(
            status,
            content=body.encode("utf-8"),
            headers=headers,
            request=request,
        )

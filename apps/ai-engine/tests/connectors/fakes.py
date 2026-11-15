"""Named fake HTTP transports for connector tests (no live network)."""

from __future__ import annotations

import httpx


class FakeBcbPtaxTransport(httpx.AsyncBaseTransport):
    """Named fake HTTP transport for the bcb_ptax connector tests.

    Records every request and replies with a canned status/body, or raises the
    configured error. Keeps tests free of live network calls (ADR-008).
    """

    def __init__(
        self,
        *,
        status: int = 200,
        body: str | bytes = "{}",
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
        content = self.body if isinstance(self.body, bytes) else self.body.encode()
        return httpx.Response(
            self.status,
            content=content,
            headers=self.headers,
            request=request,
        )

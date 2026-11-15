"""Typed errors for the BCB PTAX connector.

The caller (core-api via ai-engine) treats every upstream response as
untrusted; each failure mode maps to a distinct exception so it can decide
whether to retry, degrade or abort the pricing stage.
"""

from __future__ import annotations


class BcbPtaxError(Exception):
    """Base class for every error raised by the bcb_ptax connector."""


class BcbPtaxConfigError(BcbPtaxError):
    """Invalid connector usage or configuration (bad arguments, timeout)."""


class BcbPtaxTransportError(BcbPtaxError):
    """Network-level failure: DNS, TLS, connect/read timeout, protocol errors."""


class BcbPtaxSchemaError(BcbPtaxError):
    """A success response body violating the expected PTAX OData shape."""


class BcbPtaxApiError(BcbPtaxError):
    """Non-200 response; carries the HTTP status."""

    def __init__(self, message: str, *, status: int) -> None:
        super().__init__(message)
        self.status = status

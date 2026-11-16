"""Shared internals for connector clients.

Private to the ``connectors`` package — nothing here is re-exported; each
connector keeps its own public surface and error types.
"""

from __future__ import annotations

import math


def parse_timeout_seconds[E: Exception](
    raw: str | None,
    *,
    env_name: str,
    default: float,
    error_type: type[E],
) -> float:
    """Parse a ``*_TIMEOUT_SECONDS`` env var into a positive float.

    ``None`` and empty strings yield ``default``; non-numeric, non-finite
    or non-positive values raise ``error_type`` naming the offending env
    var. Each connector passes its own ``*ConfigError`` so error typing
    stays connector-local.

    Example:
        >>> parse_timeout_seconds(
        ...     "5",
        ...     env_name="HAPAG_TIMEOUT_SECONDS",
        ...     default=30.0,
        ...     error_type=ValueError,
        ... )
        5.0
    """
    if not raw:
        return default
    try:
        timeout = float(raw)
    except ValueError as exc:
        raise error_type(
            f"{env_name} must be a number of seconds, got {raw!r}"
        ) from exc
    if not math.isfinite(timeout) or timeout <= 0:
        raise error_type(f"{env_name} must be a finite number > 0, got {raw!r}")
    return timeout

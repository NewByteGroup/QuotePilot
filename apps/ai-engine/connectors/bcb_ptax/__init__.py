"""BCB PTAX connector: official daily USD-BRL quote (CotacaoDolarDia).

Public surface: ``BcbPtaxClient``, its ``PtaxResult``, the ``PtaxRate`` /
``PtaxResponse`` models and typed errors. Responses are untrusted input,
validated into pydantic models; money arrives as ``Decimal``.
"""

from connectors.bcb_ptax.client import (
    DEFAULT_TIMEOUT_SECONDS,
    PROD_BASE_URL,
    BcbPtaxClient,
    PtaxResult,
)
from connectors.bcb_ptax.errors import (
    BcbPtaxApiError,
    BcbPtaxConfigError,
    BcbPtaxError,
    BcbPtaxSchemaError,
    BcbPtaxTransportError,
)
from connectors.bcb_ptax.models import PtaxRate, PtaxResponse

__all__ = [
    "DEFAULT_TIMEOUT_SECONDS",
    "PROD_BASE_URL",
    "BcbPtaxApiError",
    "BcbPtaxClient",
    "BcbPtaxConfigError",
    "BcbPtaxError",
    "BcbPtaxSchemaError",
    "BcbPtaxTransportError",
    "PtaxRate",
    "PtaxResponse",
    "PtaxResult",
]

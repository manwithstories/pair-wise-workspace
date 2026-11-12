"""Connector protocol plus shared column-type inference."""
from __future__ import annotations

import math
import numbers
import re
from typing import Iterator, Protocol

import pandas as pd

TYPE_NAMES = ("decimal", "float", "int", "datetime", "string")

_INT_RE = re.compile(r"^[+-]?\d+$")
_THOUSANDS_RE = re.compile(r"^[+-]?\d{1,3}(,\d{3})+(\.\d+)?([eE][+-]?\d+)?$")
_FULLWIDTH = str.maketrans("０１２３４５６７８９．－", "0123456789.-")


class Connector(Protocol):
    """Uniform read interface implemented by every source connector."""

    name: str

    def schema(self) -> dict:
        """Return {column: declared type name} for this source."""

    def iter_batches(self, batch_size: int) -> Iterator[pd.DataFrame]:
        """Yield raw batches of at most batch_size rows."""

    def close(self) -> None:
        """Release any held resources."""


def _is_null(value) -> bool:
    if value is None:
        return True
    if isinstance(value, float) and math.isnan(value):
        return True
    return str(value).strip() == ""


def clean_numeric_text(value):
    """Light numeric normalisation shared with the transform layer."""
    if isinstance(value, bool):
        return int(value)
    if isinstance(value, numbers.Real):
        return value
    text = str(value).strip().translate(_FULLWIDTH)
    negative = False
    if text.startswith("(") and text.endswith(")"):
        negative = True
        text = text[1:-1].strip()
    if _THOUSANDS_RE.match(text):
        text = text.replace(",", "")
    return "-" + text if negative else text


def _floatable(value) -> bool:
    if isinstance(value, numbers.Real):
        return True
    try:
        float(value)
        return True
    except (TypeError, ValueError):
        return False


def infer_series_type(series: pd.Series) -> str:
    """Infer one of TYPE_NAMES from a sample of raw values."""
    vals = [v for v in series.tolist() if not _is_null(v)]
    if not vals:
        return "string"
    if all(isinstance(v, bool) for v in vals):
        return "int"
    if all(isinstance(v, numbers.Integral) and not isinstance(v, bool) for v in vals):
        return "int"
    if all(isinstance(v, numbers.Real) and not isinstance(v, bool) for v in vals):
        return "decimal"
    if any(isinstance(v, (list, tuple, dict)) for v in vals):
        return "string"
    cleaned = [clean_numeric_text(str(v).strip()) for v in vals]
    if all(isinstance(c, str) and _INT_RE.match(c) for c in cleaned):
        return "int"
    if all(_floatable(c) for c in cleaned):
        return "decimal"
    parsed = pd.to_datetime(pd.Series([str(v).strip() for v in vals]),
                            format="mixed", errors="coerce")
    if parsed.notna().all():
        return "datetime"
    return "string"

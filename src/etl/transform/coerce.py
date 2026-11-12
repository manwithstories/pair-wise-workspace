"""Conversion ladder with stable, greppable reason codes.

Reason codes (stable API, safe to grep):
  invalid_date   - unparseable date/timestamp
  enum_unmapped  - raw value missing from the enum dictionary
  overflow       - numeric value outside int64 / finite range
  not_a_number   - non-numeric text in a numeric field
  array_rejected - JSON array/object where a scalar was expected
  missing_pk     - primary key is null
"""
from __future__ import annotations

import math
import re
from collections import Counter
from dataclasses import dataclass, field

import pandas as pd

REASON_INVALID_DATE = "invalid_date"
REASON_ENUM_UNMAPPED = "enum_unmapped"
REASON_OVERFLOW = "overflow"
REASON_NOT_A_NUMBER = "not_a_number"
REASON_ARRAY_REJECTED = "array_rejected"
REASON_MISSING_PK = "missing_pk"

# Canonical display order for reason rollups (ties broken by this order).
REASON_ORDER = [
    REASON_INVALID_DATE,
    REASON_ENUM_UNMAPPED,
    REASON_OVERFLOW,
    REASON_NOT_A_NUMBER,
    REASON_ARRAY_REJECTED,
    REASON_MISSING_PK,
]

INT64_MAX = 2**63 - 1

_FULLWIDTH = str.maketrans("０１２３４５６７８９．－", "0123456789.-")
_THOUSANDS_RE = re.compile(r"^[+-]?\d{1,3}(,\d{3})+(\.\d+)?([eE][+-]?\d+)?$")
_EPOCH_RE = re.compile(r"^\d{9,11}(\.\d+)?$")


class CoercionError(Exception):
    """Raised in fail-fast mode; message carries full failure context."""

    def __init__(self, failure: "Failure"):
        self.failure = failure
        super().__init__(
            f"source={failure.source} column={failure.column} "
            f"reason={failure.reason} raw_value={failure.raw_value!r} pk={failure.pk}"
        )


@dataclass
class Failure:
    source: str
    column: str
    reason: str
    raw_value: str
    pk: str


@dataclass
class FailureCollector:
    records: list = field(default_factory=list)

    def add(self, failure: Failure) -> None:
        self.records.append(failure)

    @property
    def failure_count(self) -> int:
        return len(self.records)

    @property
    def row_count(self) -> int:
        return len({(f.source, f.pk) for f in self.records})

    def reason_counts(self) -> dict:
        counts = {}
        for f in self.records:
            counts[f.reason] = counts.get(f.reason, 0) + 1
        return counts

    def top_raw_values(self, reason: str, n: int = 3):
        counts = Counter(f.raw_value for f in self.records if f.reason == reason)
        return sorted(counts.items(), key=lambda kv: (-kv[1], kv[0]))[:n]


def null_kind(value):
    """Three-state null detection: '' vs 'NA' vs 'NULL'/native null."""
    if value is None:
        return "null"
    if isinstance(value, float) and math.isnan(value):
        return "null"
    text = str(value).strip()
    if text == "":
        return "empty"
    if text.upper() == "NA":
        return "na"
    if text.upper() == "NULL":
        return "null"
    return None


def clean_numeric_text(value):
    """Handle thousands separators, (negative), full-width digits, sci-notation."""
    if isinstance(value, bool):
        return int(value)
    if isinstance(value, (int, float)):
        return value
    text = str(value).strip().translate(_FULLWIDTH)
    negative = False
    if text.startswith("(") and text.endswith(")"):
        negative = True
        text = text[1:-1].strip()
    if _THOUSANDS_RE.match(text):
        text = text.replace(",", "")
    return "-" + text if negative else text


def _is_array(value) -> bool:
    return isinstance(value, (list, tuple, dict))


def _coerce_series(raw: pd.Series, fp, binding, enum_dicts):
    """Coerce one bound column. Returns (series, failures, null_kinds)."""
    failures = []  # list[(index_label, reason, raw_value)]
    s = raw

    if s.dtype == object:
        array_mask = s.map(_is_array)
        if array_mask.any():
            for idx in s.index[array_mask]:
                failures.append((idx, REASON_ARRAY_REJECTED, s.loc[idx]))
            s = s.mask(array_mask, None)

    kinds = s.map(null_kind)
    s = s.mask(kinds.notna(), None)
    nonnull = s.notna()
    result = pd.Series([None] * len(s), index=s.index, dtype=object)

    if fp.enum is not None:
        mapping = enum_dicts.get(fp.enum, {})
        vals = s[nonnull]
        mapped = vals.map(lambda v: mapping.get(str(v).strip()))
        for idx in vals.index[mapped.isna()]:
            failures.append((idx, REASON_ENUM_UNMAPPED, vals.loc[idx]))
        hits = mapped.dropna()
        result.loc[hits.index] = hits
        return result, failures, kinds

    ftype = fp.target_type
    if ftype in ("int", "decimal", "float"):
        vals = s[nonnull]
        cleaned = vals.map(clean_numeric_text)
        numeric = pd.to_numeric(cleaned, errors="coerce")
        nan_mask = numeric.isna()
        for idx in vals.index[nan_mask]:
            failures.append((idx, REASON_NOT_A_NUMBER, vals.loc[idx]))
        ok = numeric[~nan_mask]
        if ftype == "int":
            over = ok.abs() > INT64_MAX
            for idx in ok.index[over]:
                failures.append((idx, REASON_OVERFLOW, vals.loc[idx]))
            ok = ok[~over]
            fractional = ok % 1 != 0
            for idx in ok.index[fractional]:
                failures.append((idx, REASON_NOT_A_NUMBER, vals.loc[idx]))
            ok = ok[~fractional].astype("int64")
        else:
            infinite = ok.abs() == float("inf")
            for idx in ok.index[infinite]:
                failures.append((idx, REASON_OVERFLOW, vals.loc[idx]))
            ok = ok[~infinite]
        if binding.unit_factor is not None:
            ok = ok * binding.unit_factor
            if ftype != "int":
                ok = ok.round(4)
        result.loc[ok.index] = ok
        return result, failures, kinds

    if ftype == "datetime":
        vals = s[nonnull]
        pieces = []
        numeric_mask = vals.map(
            lambda v: isinstance(v, (int, float)) and not isinstance(v, bool)
        )
        if numeric_mask.any():
            pieces.append(pd.to_datetime(
                vals[numeric_mask].astype(float), unit="s", errors="coerce"))
        text = vals[~numeric_mask].astype(str).str.strip()
        if len(text):
            epoch_mask = text.str.fullmatch(_EPOCH_RE).fillna(False)
            if epoch_mask.any():
                pieces.append(pd.to_datetime(
                    pd.to_numeric(text[epoch_mask]), unit="s", errors="coerce"))
            rest = text[~epoch_mask]
            if len(rest):
                pieces.append(pd.to_datetime(rest, format="mixed", errors="coerce"))
        if pieces:
            parsed = pd.concat(pieces).reindex(vals.index)
        else:
            parsed = pd.Series(pd.NaT, index=vals.index)
        bad = parsed.isna()
        for idx in vals.index[bad]:
            failures.append((idx, REASON_INVALID_DATE, vals.loc[idx]))
        good = parsed[~bad]
        result.loc[good.index] = good.dt.strftime("%Y-%m-%d %H:%M:%S")
        return result, failures, kinds

    # string
    vals = s[nonnull]
    result.loc[vals.index] = vals.map(lambda v: str(v).strip())
    return result, failures, kinds


def coerce_batch(batch: pd.DataFrame, source_name: str, plan, enum_dicts,
                 collector: FailureCollector, policy: str):
    """Coerce one raw batch. Returns (clean_frame, null_kind_counts)."""
    out = {}
    bad_rows = set()
    null_counts = Counter()

    pk_binding = plan.fields[plan.pk_field].bindings.get(source_name)
    if pk_binding is not None and pk_binding.column in batch.columns:
        pk_raw = batch[pk_binding.column]
    else:
        pk_raw = pd.Series([None] * len(batch), index=batch.index)

    for field_name, fp in plan.fields.items():
        binding = fp.bindings.get(source_name)
        if binding is None:
            out[field_name] = pd.Series(
                [fp.default] * len(batch), index=batch.index, dtype=object)
            continue
        if binding.column in batch.columns:
            raw = batch[binding.column]
        else:
            raw = pd.Series([None] * len(batch), index=batch.index)
        series, failures, kinds = _coerce_series(raw, fp, binding, enum_dicts)
        null_counts.update(k for k in kinds if k)
        for idx, reason, raw_value in failures:
            pk_val = pk_raw.loc[idx] if idx in pk_raw.index else None
            if pk_val is None or (isinstance(pk_val, float) and math.isnan(pk_val)):
                pk_text = f"{source_name}#{idx}"
            else:
                pk_text = str(pk_val)
            failure = Failure(
                source=source_name,
                column=field_name,
                reason=reason,
                raw_value="" if raw_value is None else str(raw_value),
                pk=pk_text,
            )
            if policy == "fail-fast":
                raise CoercionError(failure)
            collector.add(failure)
            bad_rows.add(idx)
        out[field_name] = series

    frame = pd.DataFrame(out)
    if bad_rows:
        frame = frame.drop(index=sorted(bad_rows))

    pk_mask = frame[plan.pk_field].isna()
    if pk_mask.any():
        for idx in frame.index[pk_mask]:
            failure = Failure(source_name, plan.pk_field, REASON_MISSING_PK,
                              "", f"{source_name}#{idx}")
            if policy == "fail-fast":
                raise CoercionError(failure)
            collector.add(failure)
        frame = frame.drop(index=frame.index[pk_mask])
    return frame, null_counts

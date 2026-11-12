from types import SimpleNamespace

import pandas as pd

from etl.config import Binding
from etl.schema.plan import build_plan
from etl.transform.coerce import (FailureCollector, clean_numeric_text,
                                  coerce_batch, null_kind)


def test_clean_numeric_thousands():
    assert clean_numeric_text("1,234,567") == "1234567"


def test_clean_numeric_parenthesised_negative():
    assert clean_numeric_text("(123)") == "-123"


def test_clean_numeric_fullwidth():
    assert clean_numeric_text("１２３４") == "1234"


def test_clean_numeric_scientific():
    assert float(clean_numeric_text("1.2e3")) == 1200.0


def test_null_kind_three_states():
    assert null_kind("") == "empty"
    assert null_kind("NA") == "na"
    assert null_kind("NULL") == "null"
    assert null_kind(None) == "null"
    assert null_kind("x") is None


def _plan(target_schema, bindings, declared):
    src = SimpleNamespace(
        name="s",
        bindings={t: Binding(column=c) for t, c in bindings.items()},
    )
    cfg = SimpleNamespace(target_schema=target_schema, sources=[src])
    return build_plan(cfg, {"s": declared})


def test_coerce_batch_reason_codes():
    schema = {
        "order_id": {"type": "string", "pk": True, "default": None, "enum": None},
        "quantity": {"type": "int", "pk": False, "default": None, "enum": None},
        "trade_date": {"type": "datetime", "pk": False, "default": None, "enum": None},
        "status": {"type": "string", "pk": False, "default": None, "enum": "status"},
    }
    plan = _plan(
        schema,
        {"order_id": "order_id", "quantity": "qty",
         "trade_date": "tdate", "status": "status"},
        {"order_id": "string", "qty": "int", "tdate": "datetime",
         "status": "string"},
    )
    batch = pd.DataFrame({
        "order_id": ["A", "B", "C", "D"],
        "qty": ["1,234", "99999999999999999999", "5", "6"],
        "tdate": ["2024/01/05", "2024/01/06", "not-a-date", "2024/01/08"],
        "status": ["P", "P", "P", "??"],
    })
    collector = FailureCollector()
    clean, nulls = coerce_batch(batch, "s", plan, {"status": {"P": "paid"}},
                                collector, "quarantine")
    reasons = sorted(f.reason for f in collector.records)
    assert reasons == ["enum_unmapped", "invalid_date", "overflow"]
    assert list(clean["order_id"]) == ["A"]
    assert clean.iloc[0]["quantity"] == 1234
    assert collector.row_count == 3


def test_coerce_batch_array_rejected():
    schema = {
        "order_id": {"type": "string", "pk": True, "default": None, "enum": None},
        "amount": {"type": "decimal", "pk": False, "default": None, "enum": None},
    }
    plan = _plan(schema, {"order_id": "order_id", "amount": "amount"},
                 {"order_id": "string", "amount": "decimal"})
    batch = pd.DataFrame({"order_id": ["A", "B"],
                          "amount": [["x", "y"], 12.5]}, dtype=object)
    collector = FailureCollector()
    clean, _ = coerce_batch(batch, "s", plan, {}, collector, "quarantine")
    assert [f.reason for f in collector.records] == ["array_rejected"]
    assert list(clean["order_id"]) == ["B"]


def test_coerce_batch_fail_fast_raises():
    schema = {
        "order_id": {"type": "string", "pk": True, "default": None, "enum": None},
        "quantity": {"type": "int", "pk": False, "default": None, "enum": None},
    }
    plan = _plan(schema, {"order_id": "order_id", "quantity": "qty"},
                 {"order_id": "string", "qty": "int"})
    batch = pd.DataFrame({"order_id": ["A"], "qty": ["abc"]})
    from etl.transform.coerce import CoercionError
    try:
        coerce_batch(batch, "s", plan, {}, FailureCollector(), "fail-fast")
    except CoercionError as exc:
        msg = str(exc)
        assert "column=quantity" in msg and "reason=not_a_number" in msg
        assert "raw_value='abc'" in msg
    else:
        raise AssertionError("fail-fast did not raise")

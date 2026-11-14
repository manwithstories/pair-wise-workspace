from __future__ import annotations

import numpy as np
import pytest

from mixer.automation import AutomationCurve, db_to_gain
from mixer.errors import ManifestError


def curve(*segments: dict) -> AutomationCurve:
    parsed = AutomationCurve.parse(list(segments), 8.0)
    assert parsed is not None
    return parsed


def test_linear_hold_and_exponential_boundaries() -> None:
    automation = curve(
        {"type": "linear", "start": 0.0, "end": 2.0, "from_db": 0.0, "to_db": -20.0},
        {"type": "hold", "start": 2.0, "end": 4.0, "from_db": -20.0, "to_db": -20.0},
        {"type": "exponential", "start": 4.0, "end": 6.0, "from_db": 0.0, "to_db": -20.0},
    )
    values = automation.values_at(np.array([-1.0, 0.0, 1.0, 2.0, 3.0, 4.0, 5.0, 6.0, 9.0]))
    assert values[0] == pytest.approx(0.0)
    assert values[1] == pytest.approx(0.0)
    assert values[2] == pytest.approx(-10.0)
    assert values[3] == pytest.approx(-20.0)
    assert values[4] == pytest.approx(-20.0)
    assert values[5] == pytest.approx(0.0)
    assert values[6] == pytest.approx(-10.0)
    assert values[7] == pytest.approx(-20.0)
    assert values[8] == pytest.approx(-20.0)


def test_control_rate_count_and_sample_interpolation() -> None:
    automation = AutomationCurve.parse(
        [{"type": "linear", "start": 0.0, "end": 1.0, "from_db": 0.0, "to_db": -10.0}],
        0.5,
    )
    assert automation is not None
    assert automation.points == 500
    # At half a millisecond the two 1 kHz control samples are 0 and -0.01 dB.
    gain_db = automation.gain_db_at_samples(np.array([24]), 48000, static_db=3.0)
    assert gain_db[0] == pytest.approx(3.0 - 0.005)
    assert db_to_gain(0.0) == pytest.approx(1.0)
    assert db_to_gain(-6.0) == pytest.approx(10.0 ** (-6.0 / 20.0))


def test_points_style_automation_and_breakpoints() -> None:
    automation = AutomationCurve.parse(
        [
            {
                "type": "linear",
                "points": [
                    {"t": 0.0, "gain_db": 0.0},
                    {"t": 1.0, "gain_db": -6.0},
                    {"t": 2.0, "gain_db": -3.0},
                ],
            }
        ],
        2.0,
    )
    assert automation is not None
    assert automation.values_at(np.array([0.5, 1.5])).tolist() == pytest.approx([-3.0, -4.5])
    assert [point["t"] for point in automation.breakpoints] == [0.0, 1.0, 2.0]


@pytest.mark.parametrize(
    "bad",
    [
        [{"type": "unknown", "start": 0.0, "end": 1.0, "from_db": 0.0, "to_db": 0.0}],
        [{"type": "linear", "start": 2.0, "end": 1.0, "from_db": 0.0, "to_db": 0.0}],
        [{"type": "linear", "start": 0.0, "end": 1.0, "from_db": float("nan"), "to_db": 0.0}],
    ],
)
def test_invalid_automation_is_structured(bad: list[dict]) -> None:
    with pytest.raises(ManifestError):
        AutomationCurve.parse(bad, 2.0)

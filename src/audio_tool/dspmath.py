"""Small numeric helpers shared by the DSP modules.

The modified Bessel function of the first kind (order zero) is what Kaiser
windows need; it is implemented by its series expansion so the tool depends on
the standard library alone.
"""

from __future__ import annotations

import math


def bessel_i0(value: float) -> float:
    """Modified Bessel function I0, unit value at zero.

    Series form ``sum_k ((x/2)^k / k!)^2``, which converges in well under 64
    terms for the beta range windows use (0..20).
    """

    total = 1.0
    term = 1.0
    half = value * 0.5
    for k in range(1, 64):
        term *= (half / k) ** 2
        total += term
        if term < 1e-17 * total:
            break
    return total


def kaiser_window(x: float, beta: float) -> float:
    """Kaiser window on ``[-1, 1]``, peaking at 1.0 at the centre.

    Values outside the support return 0, so callers can multiply blindly.
    """

    if x <= -1.0 or x >= 1.0:
        return 0.0
    return bessel_i0(beta * math.sqrt(1.0 - x * x)) / bessel_i0(beta)


def sinc(x: float) -> float:
    """Normalised sinc, ``sin(pi*x)/(pi*x)``, unit value at zero."""

    if x == 0.0:
        return 1.0
    arg = math.pi * x
    return math.sin(arg) / arg

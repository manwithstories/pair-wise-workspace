"""Interpolation curves: linear, step, ease-in-out, cubic bezier."""

from .errors import TimelineError


def linear(u):
    return u


def step(u):
    return 0.0 if u < 1.0 else 1.0


def ease_in_out(u):
    return u * u * (3.0 - 2.0 * u)


def _bezier_point(a, b, c, d, t):
    mt = 1.0 - t
    return mt * mt * mt * a + 3.0 * mt * mt * t * b + 3.0 * mt * t * t * c + t * t * t * d


def cubic_bezier(x1, y1, x2, y2):
    """CSS-style cubic-bezier easing; x is solved from u by bisection."""
    for name, val in (("x1", x1), ("x2", x2)):
        if not 0.0 <= val <= 1.0:
            raise TimelineError(
                "bezier %s must be within [0, 1], got %r" % (name, val))

    def ease(u):
        if u <= 0.0:
            return 0.0
        if u >= 1.0:
            return 1.0
        lo, hi = 0.0, 1.0
        t = u
        for _ in range(48):
            x = _bezier_point(0.0, x1, x2, 1.0, t)
            if x < u:
                lo = t
            else:
                hi = t
            t = 0.5 * (lo + hi)
        return _bezier_point(0.0, y1, y2, 1.0, t)

    return ease


_NAMED = {
    "linear": linear,
    "step": step,
    "ease-in-out": ease_in_out,
}


def get_easing(spec, location=None):
    """Resolve an easing spec (name string or {"bezier": [x1,y1,x2,y2]})."""
    if spec is None:
        return linear
    if isinstance(spec, str):
        fn = _NAMED.get(spec)
        if fn is None:
            raise TimelineError(
                "unknown easing %r (expected one of %s or a bezier spec)"
                % (spec, sorted(_NAMED)), location=location)
        return fn
    if isinstance(spec, dict) and "bezier" in spec:
        pts = spec["bezier"]
        if not isinstance(pts, (list, tuple)) or len(pts) != 4:
            raise TimelineError(
                "bezier easing needs exactly 4 numbers", location=location)
        try:
            x1, y1, x2, y2 = (float(v) for v in pts)
        except (TypeError, ValueError):
            raise TimelineError(
                "bezier easing values must be numbers", location=location)
        return cubic_bezier(x1, y1, x2, y2)
    raise TimelineError("invalid easing spec %r" % (spec,), location=location)

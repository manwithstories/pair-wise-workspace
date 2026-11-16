"""ABR (adaptive bitrate) engine — local, deterministic ABR simulation.

Data flow:
    trace.py -> estimator.py -> policy.py (+ simulator.py buffer state)
    -> metrics.py -> io.py / __main__.py
"""

__all__ = ["__version__"]
__version__ = "1.0.0"

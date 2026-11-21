"""Pytest configuration: make the synthetic fixture helpers importable.

The fixture generators live under ``tests/`` (not inside the installed
package) because they are test-only synthesis code.  This conftest puts the
tests directory on ``sys.path`` so ``import fixtures`` / ``fixtures_core``
works regardless of the invocation directory.
"""

import os
import sys

TESTS_DIR = os.path.dirname(os.path.abspath(__file__))
if TESTS_DIR not in sys.path:
    sys.path.insert(0, TESTS_DIR)

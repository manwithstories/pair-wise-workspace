"""Repo-local import shim.

The project is not pip-installed in this workspace, so this shim extends the
package search path to ``src/plateqc`` allowing ``python -m plateqc.cli`` and
``import plateqc.<module>`` to work directly from the repository root.
"""

import os

_src_pkg = os.path.join(
    os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "src", "plateqc"
)
if os.path.isdir(_src_pkg):
    __path__.append(_src_pkg)

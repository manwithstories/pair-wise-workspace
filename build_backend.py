"""Custom PEP 517 build backend for the ``avtimeline`` project.

The acceptance environment is offline and the interpreter's own site-packages
does not ship ``setuptools``, so the project builds its wheel directly.  The
backend is intentionally tiny: it copies ``src/avtimeline`` into a wheel and
writes an editable ``.pth`` file that points at the same tree.
"""

from __future__ import annotations

import base64
import csv
import hashlib
import io
import os
import tempfile
import zipfile
from pathlib import Path

NAME = "avtimeline"
DIST = "avtimeline"
VERSION = "0.1.0"
DIST_INFO = f"{DIST}-{VERSION}.dist-info"

ENTRY_POINTS = "[console_scripts]\navtimeline = avtimeline.cli:main\n"

METADATA = (
    "Metadata-Version: 2.1\n"
    f"Name: {NAME}\n"
    f"Version: {VERSION}\n"
    "Summary: Deterministic multi-track media timeline segmentation\n"
    "Requires-Python: >=3.12\n"
    "Provides-Extra: test\n"
    "\n"
)

WHEEL = (
    "Wheel-Version: 1.0\n"
    "Generator: avtimeline-local-backend\n"
    "Root-Is-Purelib: true\n"
    "Tag: py3-none-any\n"
)


def _write_distinfo(directory: Path) -> str:
    base = directory / DIST_INFO
    base.mkdir(parents=True, exist_ok=True)
    (base / "METADATA").write_text(METADATA, encoding="utf-8")
    (base / "WHEEL").write_text(WHEEL, encoding="utf-8")
    (base / "entry_points.txt").write_text(ENTRY_POINTS, encoding="utf-8")
    (base / "top_level.txt").write_text(f"{DIST}\n", encoding="utf-8")
    return DIST_INFO


def get_requires_for_build_wheel(config_settings=None):
    return []


def get_requires_for_build_editable(config_settings=None):
    return []


def get_requires_for_build_sdist(config_settings=None):
    return []


def prepare_metadata_for_build_wheel(metadata_directory, config_settings=None):
    return _write_distinfo(Path(metadata_directory))


def prepare_metadata_for_build_editable(metadata_directory, config_settings=None):
    return _write_distinfo(Path(metadata_directory))


def _hash(data: bytes) -> str:
    digest = base64.urlsafe_b64encode(hashlib.sha256(data).digest()).decode()
    return "sha256=" + digest.rstrip("=")


def _record(entries) -> bytes:
    out = io.StringIO()
    writer = csv.writer(out, lineterminator="\n")
    for name, data in entries:
        writer.writerow([name, _hash(data), str(len(data))])
    writer.writerow([f"{DIST_INFO}/RECORD", "", ""])
    return out.getvalue().encode()


def _build_wheel(wheel_directory, files, distinfo_files) -> str:
    entries = [
        (name if name.endswith(".pth") else f"{DIST}/{name}", data)
        for name, data in files
    ]
    entries += [(f"{DIST_INFO}/{name}", data) for name, data in distinfo_files]
    wheel_name = f"{DIST}-{VERSION}-py3-none-any.whl"
    path = Path(wheel_directory) / wheel_name
    with zipfile.ZipFile(path, "w", zipfile.ZIP_DEFLATED) as archive:
        for name, data in entries:
            archive.writestr(name, data)
        archive.writestr(f"{DIST_INFO}/RECORD", _record(entries))
    return wheel_name


def _collect_distinfo(metadata_directory=None):
    if metadata_directory:
        base = Path(metadata_directory) / DIST_INFO
        if base.is_dir():
            return sorted((p.name, p.read_bytes()) for p in base.iterdir() if p.is_file())
    with tempfile.TemporaryDirectory() as tmp:
        _write_distinfo(Path(tmp))
        base = Path(tmp) / DIST_INFO
        return sorted((p.name, p.read_bytes()) for p in base.iterdir() if p.is_file())


def build_wheel(wheel_directory, config_settings=None, metadata_directory=None):
    root = Path("src") / DIST
    files = []
    for path in sorted(root.rglob("*")):
        if path.is_file() and "__pycache__" not in path.parts:
            files.append((str(path.relative_to(root)), path.read_bytes()))
    return _build_wheel(wheel_directory, files, _collect_distinfo(metadata_directory))


def build_editable(wheel_directory, config_settings=None, metadata_directory=None):
    src = os.path.abspath("src")
    files = [(f"{DIST}.pth", (src + os.linesep).encode())]
    return _build_wheel(wheel_directory, files, _collect_distinfo(metadata_directory))


def build_sdist(sdist_directory, config_settings=None):
    raise NotImplementedError("sdist is not produced by the local backend")
